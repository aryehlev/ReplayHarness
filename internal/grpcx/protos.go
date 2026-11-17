package grpcx

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ProtoSet is what Compile found.
type ProtoSet struct {
	Files *protoregistry.Files
	Own   map[string]string // import name of each project file -> its path on disk
	Notes []string          // files that could not be compiled, and why
}

var (
	skipProtoDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "dist": true, "build": true, "target": true, ".cache": true}
	depProtoDirs  = map[string]bool{"vendor": true, "third_party": true, "third-party": true, "external": true, "googleapis": true}
	packageRe     = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;`)
)

// FindProtos lists the .proto files under the paths, split into the
// project's own files and dependency copies (vendor/, third_party/).
func FindProtos(paths []string) (own, deps []string) {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if strings.HasSuffix(p, ".proto") {
				own = append(own, p)
			}
			continue
		}
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != p && skipProtoDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".proto") {
				return nil
			}
			rel, _ := filepath.Rel(p, path)
			dep := false
			for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
				if depProtoDirs[part] {
					dep = true
				}
			}
			if dep {
				deps = append(deps, path)
			} else {
				own = append(own, path)
			}
			return nil
		})
	}
	sort.Strings(own)
	sort.Strings(deps)
	return own, deps
}

// importName guesses the name other files import a .proto file by: its path
// below the directory that matches its package (proto/users/v1/users.proto
// in package users.v1 is users/v1/users.proto, imported from proto/).
func importName(path string) (root, name string) {
	dir, file := filepath.Split(path)
	dir = filepath.Clean(dir)
	src, err := os.ReadFile(path)
	if err == nil {
		if m := packageRe.FindSubmatch(src); m != nil {
			pkgDir := filepath.FromSlash(strings.ReplaceAll(string(m[1]), ".", "/"))
			if dir == pkgDir {
				return ".", filepath.ToSlash(filepath.Join(pkgDir, file))
			}
			if strings.HasSuffix(dir, string(filepath.Separator)+pkgDir) {
				return strings.TrimSuffix(dir, string(filepath.Separator)+pkgDir), filepath.ToSlash(filepath.Join(pkgDir, file))
			}
		}
	}
	return dir, file
}

// Compile compiles the .proto files under paths. Files that do not compile
// (usually because an import is missing) are skipped with a note.
func Compile(paths []string) ProtoSet {
	set := ProtoSet{Files: new(protoregistry.Files), Own: map[string]string{}}
	own, deps := FindProtos(paths)
	if len(own) == 0 {
		return set
	}
	rootSet := map[string]bool{}
	var roots []string
	addRoot := func(r string) {
		if !rootSet[r] {
			rootSet[r] = true
			roots = append(roots, r)
		}
	}
	type entry struct{ name, path string }
	var entries []entry
	for _, f := range own {
		root, name := importName(f)
		addRoot(root)
		entries = append(entries, entry{name, f})
	}
	for _, f := range deps {
		root, _ := importName(f)
		addRoot(root)
	}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			addRoot(p)
		}
	}
	// Imports are often written relative to a parent of the file's directory.
	for _, f := range append(append([]string(nil), own...), deps...) {
		for _, p := range paths {
			if rel, err := filepath.Rel(p, f); err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			for dir := filepath.Dir(f); ; dir = filepath.Dir(dir) {
				addRoot(dir)
				if dir == filepath.Clean(p) || dir == filepath.Dir(dir) {
					break
				}
			}
		}
	}

	resolver := protocompile.WithStandardImports(protocompile.CompositeResolver{
		&protocompile.SourceResolver{ImportPaths: roots},
		protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
			if src, ok := stubs[path]; ok {
				return protocompile.SearchResult{Source: strings.NewReader(src)}, nil
			}
			return protocompile.SearchResult{}, fmt.Errorf("import %q not found: %w", path, protoregistry.NotFound)
		}),
	})
	comp := protocompile.Compiler{Resolver: resolver, SourceInfoMode: protocompile.SourceInfoStandard}
	var register func(fd protoreflect.FileDescriptor)
	register = func(fd protoreflect.FileDescriptor) {
		if _, err := set.Files.FindFileByPath(fd.Path()); err == nil {
			return
		}
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			register(imports.Get(i).FileDescriptor)
		}
		_ = set.Files.RegisterFile(fd)
	}
	for _, e := range entries {
		files, err := comp.Compile(context.Background(), e.name)
		if err != nil {
			set.Notes = append(set.Notes, fmt.Sprintf("skipped %s: %v", e.path, firstLine(err.Error())))
			continue
		}
		for _, fd := range files {
			if _, err := set.Files.FindFileByPath(fd.Path()); err == nil {
				set.Own[fd.Path()] = e.path
				continue
			}
			imports := fd.Imports()
			for i := 0; i < imports.Len(); i++ {
				register(imports.Get(i).FileDescriptor)
			}
			if err := set.Files.RegisterFile(fd); err != nil {
				set.Notes = append(set.Notes, fmt.Sprintf("skipped %s: %v", e.path, err))
				continue
			}
			set.Own[fd.Path()] = e.path
		}
	}
	return set
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ServicesIn lists the services in the registry, only from the files in own
// when own is not nil.
func ServicesIn(files *protoregistry.Files, own map[string]string) []protoreflect.ServiceDescriptor {
	var out []protoreflect.ServiceDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if own != nil {
			if _, ok := own[fd.Path()]; !ok {
				return true
			}
		}
		if strings.HasPrefix(fd.Path(), "google/") {
			return true
		}
		for i := 0; i < fd.Services().Len(); i++ {
			out = append(out, fd.Services().Get(i))
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out
}

// stubs stand in for the googleapis files that services import for HTTP
// annotations and field behavior, so those files compile without a copy of
// googleapis.
var stubs = map[string]string{
	"google/api/http.proto": `syntax = "proto3";
package google.api;
message Http { repeated HttpRule rules = 1; bool fully_decode_reserved_expansion = 2; }
message HttpRule {
  string selector = 1;
  oneof pattern { string get = 2; string put = 3; string post = 4; string delete = 5; string patch = 6; CustomHttpPattern custom = 8; }
  string body = 7;
  string response_body = 12;
  repeated HttpRule additional_bindings = 11;
}
message CustomHttpPattern { string kind = 1; string path = 2; }
`,
	"google/api/annotations.proto": `syntax = "proto3";
package google.api;
import "google/api/http.proto";
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { HttpRule http = 72295728; }
`,
	"google/api/field_behavior.proto": `syntax = "proto3";
package google.api;
import "google/protobuf/descriptor.proto";
extend google.protobuf.FieldOptions { repeated google.api.FieldBehavior field_behavior = 1052 [packed = false]; }
enum FieldBehavior {
  FIELD_BEHAVIOR_UNSPECIFIED = 0; OPTIONAL = 1; REQUIRED = 2; OUTPUT_ONLY = 3; INPUT_ONLY = 4;
  IMMUTABLE = 5; UNORDERED_LIST = 6; NON_EMPTY_DEFAULT = 7; IDENTIFIER = 8;
}
`,
	"google/api/client.proto": `syntax = "proto3";
package google.api;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { repeated string method_signature = 1051; }
extend google.protobuf.ServiceOptions { string default_host = 1049; string oauth_scopes = 1050; string api_version = 525000001; }
`,
}
