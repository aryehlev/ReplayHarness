package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/aryehlev/ReplayHarness/internal/gql"
	"github.com/aryehlev/ReplayHarness/internal/grpcx"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// scanProtos compiles the project's .proto files and returns one endpoint
// per gRPC method.
func scanProtos(root string) ([]*model.Endpoint, []string) {
	set := grpcx.Compile([]string{root})
	if len(set.Own) == 0 {
		return nil, set.Notes
	}
	services := grpcx.ServicesIn(set.Files, set.Own)
	eps := grpcx.Endpoints(services, func(md protoreflect.MethodDescriptor) string {
		return grpcx.Location(md, set.Own, root)
	})
	return eps, set.Notes
}

var (
	sdlExts = map[string]bool{".graphql": true, ".graphqls": true, ".gql": true}
	// SDL inside JavaScript and TypeScript: gql`...`, graphql`...`, buildSchema(`...`), typeDefs = `...`.
	embeddedSDLRe = regexp.MustCompile("(?s)(?:\\bgql|\\bgraphql|buildSchema\\(|typeDefs\\s*[:=])\\s*`([^`]*)`")
	interpRe      = regexp.MustCompile(`\$\{[^}]*\}`)
	sdlRootRe     = regexp.MustCompile(`(?m)^\s*(?:extend\s+)?(?:type\s+(?:Query|Mutation|Subscription)\b|schema\s*\{)`)
)

// scanSDL finds GraphQL schemas: .graphql, .graphqls and .gql files, and
// schemas written inline in JavaScript or TypeScript.
func scanSDL(root string) (*gql.Schema, []string, []string) {
	var sources []*ast.Source
	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || skipCodeDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 4<<20 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		rel := relPath(root, path)
		switch {
		case sdlExts[ext]:
			data, err := os.ReadFile(path)
			if err == nil {
				sources = append(sources, &ast.Source{Name: rel, Input: string(data)})
				files = append(files, rel)
			}
		case codeExts[ext] && !isTestFile(d.Name()) && (ext == ".js" || ext == ".ts" || ext == ".mjs" || ext == ".cjs" || ext == ".jsx" || ext == ".tsx"):
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte("type ")) {
				return nil
			}
			for _, m := range embeddedSDLRe.FindAllSubmatchIndex(data, -1) {
				body := string(data[m[2]:m[3]])
				if !sdlRootRe.MatchString(body) && !strings.Contains(body, "type ") {
					continue
				}
				// Keep line numbers right: pad with the newlines before the literal.
				pad := strings.Repeat("\n", bytes.Count(data[:m[2]], []byte("\n")))
				sources = append(sources, &ast.Source{Name: rel, Input: pad + interpRe.ReplaceAllString(body, "")})
				files = append(files, rel)
			}
		}
		return nil
	})
	if len(sources) == 0 {
		return nil, nil, nil
	}
	schema, notes := gql.ParseSDL(sources)
	sort.Strings(files)
	return schema, notes, dedupe(files)
}

func dedupe(list []string) []string {
	out := list[:0]
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// graphqlPath picks where the GraphQL endpoint is served from the routes in
// the code: a route with "graphql" in it, or gqlgen's /query.
func graphqlPath(routes []route) string {
	for _, r := range routes {
		if strings.Contains(strings.ToLower(r.path), "graphql") && !strings.Contains(strings.ToLower(r.path), "playground") {
			return r.path
		}
	}
	for _, r := range routes {
		if r.path == "/query" {
			return r.path
		}
	}
	return "/graphql"
}

// GraphQLPaths are where GraphQL servers usually listen.
var GraphQLPaths = []string{"/graphql", "/query", "/api/graphql", "/graphql/", "/gql"}

// GraphQLFromServer introspects a running GraphQL server, trying each path
// until one answers.
func GraphQLFromServer(ctx context.Context, client *http.Client, base string, paths []string, headers map[string]string) (*model.API, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	body := gql.Envelope(gql.IntrospectionQuery, nil, "")
	var lastErr error
	for _, p := range paths {
		target := strings.TrimRight(u.String(), "/") + p
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if !json.Valid(data) {
			lastErr = fmt.Errorf("%s: status %d, not JSON", target, resp.StatusCode)
			continue
		}
		schema, err := gql.FromIntrospection(data)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", target, err)
			continue
		}
		api := &model.API{Title: "GraphQL API", Sources: []string{"GraphQL introspection at " + target}, Endpoints: schema.Endpoints(p)}
		return api, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no GraphQL endpoint answered")
	}
	return nil, lastErr
}

// GRPCFromServer lists the services of a running gRPC server through
// reflection, falling back to the .proto files the client was given.
func GRPCFromServer(ctx context.Context, g *grpcx.Client, root string) (*model.API, error) {
	services, source, notes, err := g.Services(ctx)
	if err != nil {
		return nil, fmt.Errorf("gRPC on %s: %w", g.Addr(), err)
	}
	api := &model.API{Title: "gRPC API", Notes: notes}
	if source != "" {
		api.Sources = append(api.Sources, source)
	}
	api.Endpoints = grpcx.Endpoints(services, func(md protoreflect.MethodDescriptor) string {
		if source != "" {
			return source
		}
		return grpcx.Location(md, nil, root)
	})
	return api, nil
}

// AddEndpoints adds the endpoints from more that api does not have yet.
func AddEndpoints(api, more *model.API) int {
	n := 0
	for _, ep := range more.Endpoints {
		if api.Endpoint(ep.Key()) == nil {
			api.Endpoints = append(api.Endpoints, ep)
			n++
		}
	}
	if n > 0 {
		api.Sources = append(api.Sources, more.Sources...)
	}
	api.Notes = append(api.Notes, more.Notes...)
	return n
}

// HasProtocol reports whether any endpoint uses the protocol.
func HasProtocol(api *model.API, proto string) bool {
	for _, ep := range api.Endpoints {
		if ep.Protocol == proto {
			return true
		}
	}
	return false
}
