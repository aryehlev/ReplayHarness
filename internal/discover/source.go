package discover

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// ScanRepo discovers the endpoints of the service in a source tree. OpenAPI or
// Swagger files are used when present; routes found in the code are compared
// with them, and routes the spec misses are added so they get tested too.
// Without a spec, the routes in the code are the endpoints.
func ScanRepo(root string) (*model.API, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	abs, _ := filepath.Abs(root)
	api := &model.API{Title: filepath.Base(abs)}

	specs := 0
	for _, f := range findSpecFiles(root) {
		rel := relPath(root, f)
		sub, err := LoadSpecFile(f)
		if err != nil {
			api.Notes = append(api.Notes, fmt.Sprintf("skipped %s: %v", rel, err))
			continue
		}
		specs++
		if specs == 1 && sub.Title != "" {
			api.Title = sub.Title
		}
		api.Sources = append(api.Sources, rel)
		for _, h := range sub.AuthHeaders {
			api.AuthHeaders = appendUnique(api.AuthHeaders, h)
		}
		for _, ep := range sub.Endpoints {
			ep.Source = rel
			if api.Endpoint(ep.Key()) == nil {
				api.Endpoints = append(api.Endpoints, ep)
			}
		}
	}

	routes := scanCode(root)

	// gRPC methods from .proto files, GraphQL root fields from schema files.
	grpcEps, notes := scanProtos(root)
	api.Notes = append(api.Notes, notes...)
	if len(grpcEps) > 0 {
		api.Endpoints = append(api.Endpoints, grpcEps...)
		api.Sources = append(api.Sources, fmt.Sprintf("%d gRPC methods in .proto files", len(grpcEps)))
	}
	if schema, notes, files := scanSDL(root); schema != nil || len(notes) > 0 {
		api.Notes = append(api.Notes, notes...)
		if schema.HasRoots() {
			api.Endpoints = append(api.Endpoints, schema.Endpoints(graphqlPath(routes))...)
			api.Sources = append(api.Sources, "GraphQL schema in "+strings.Join(files, ", "))
		}
	}

	// The API map written by the discovery agent adds handlers and rules to
	// the spec, or stands in for one.
	var fromMap map[*model.Endpoint]bool
	if mf := findMapFile(root); mf != "" {
		m, err := LoadSpecFile(mf)
		if err != nil {
			return nil, err
		}
		rel := relPath(root, mf)
		for _, ep := range m.Endpoints {
			ep.Source = rel
		}
		if specs == 0 && m.Title != "" {
			api.Title = m.Title
		}
		api.Sources = append(api.Sources, rel)
		fromMap = mergeMap(api, m)
	}

	if specs == 0 && fromMap == nil {
		added := 0
		for _, r := range routes {
			if servesProtocol(api, r) {
				continue
			}
			api.Endpoints = append(api.Endpoints, routeEndpoint(r))
			added++
		}
		if added > 0 {
			api.Sources = append(api.Sources, fmt.Sprintf("%d routes in source code", added))
		}
		return api, nil
	}
	if len(routes) > 0 {
		computeDrift(api, routes)
	}
	return api, nil
}

type route struct {
	method string // "" means any method
	path   string
	types  map[string]string // path parameter name -> integer, number or uuid
	where  string
}

type rawRoute struct {
	method string
	path   string
	offset int
}

var (
	skipDirs = map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, "target": true,
		".venv": true, "venv": true, "__pycache__": true, ".next": true, ".nuxt": true, ".idea": true,
		".vscode": true, ".gradle": true, "bin": true, "obj": true, "coverage": true, ".terraform": true,
		".cache": true, ".tox": true, "site-packages": true,
	}
	skipCodeDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true, "e2e": true, "testdata": true, "fixtures": true}
	specSniffRe  = regexp.MustCompile(`(?m)^[\s{]*["']?(?:openapi|swagger)["']?\s*:\s*["']?[23]\.`)
)

func findSpecFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".json", ".yaml", ".yml":
		default:
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 20<<20 {
			return nil
		}
		if sniff(path) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func sniff(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := bufio.NewReader(f).Read(buf)
	return specSniffRe.Match(buf[:n])
}

func scanCode(root string) []route {
	var routes []route
	seen := map[string]bool{}
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
		if isTestFile(d.Name()) {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 2<<20 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !codeExts[ext] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel := relPath(root, path)
		for _, r := range extractRoutes(rel, string(data)) {
			key := r.method + " " + r.path
			if !seen[key] {
				seen[key] = true
				routes = append(routes, r)
			}
		}
		return nil
	})
	return routes
}

var codeExts = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".mts": true, ".cts": true, ".jsx": true, ".tsx": true,
	".py": true, ".go": true, ".java": true, ".kt": true, ".rb": true, ".rs": true, ".php": true,
}

func isTestFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, "_test.go") || strings.Contains(lower, ".test.") || strings.Contains(lower, ".spec.") ||
		strings.HasPrefix(lower, "test_") || strings.HasSuffix(lower, "_test.py") || strings.HasSuffix(lower, "_spec.rb") ||
		strings.HasSuffix(name, "Test.java") || strings.HasSuffix(name, "Tests.java") || strings.HasSuffix(name, "Test.kt")
}

// extractRoutes finds route registrations in one source file.
func extractRoutes(rel, src string) []route {
	base := filepath.Base(rel)
	var raws []rawRoute
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".jsx", ".tsx":
		raws = jsRoutes(src)
		if strings.Contains(src, "@Controller") {
			raws = append(raws, nestRoutes(src)...)
		}
	case ".py":
		raws = pythonRoutes(src)
		if base == "urls.py" {
			raws = append(raws, djangoRoutes(src)...)
		}
	case ".go":
		raws = goRoutes(src)
	case ".java", ".kt":
		raws = springRoutes(src)
	case ".rb":
		if base == "routes.rb" {
			raws = railsRoutes(src)
		}
	case ".rs":
		raws = rustRoutes(src)
	case ".php":
		raws = laravelRoutes(src)
	}
	out := make([]route, 0, len(raws))
	for _, r := range raws {
		path, types := normalizePath(r.path)
		method := strings.ToUpper(r.method)
		if method == "ALL" || method == "ANY" {
			method = ""
		}
		line := 1 + strings.Count(src[:r.offset], "\n")
		out = append(out, route{method: method, path: path, types: types, where: fmt.Sprintf("%s:%d", rel, line)})
	}
	return out
}

const quote = `['"` + "`" + `]`
const notQuote = `[^'"` + "`" + `]`

var (
	jsRouteRe        = regexp.MustCompile(`\b(?:\w*[Rr]outer|\w*[Aa]pp|server|routes|route|r|fastify|hono)\s*\.\s*(get|post|put|patch|delete|head|options|all)\s*\(\s*` + quote + `(/` + notQuote + `*)` + quote)
	nestControllerRe = regexp.MustCompile(`@Controller\(\s*(?:` + quote + `(` + notQuote + `*)` + quote + `)?`)
	nestRouteRe      = regexp.MustCompile(`@(Get|Post|Put|Patch|Delete|Head|Options|All)\(\s*(?:` + quote + `(` + notQuote + `*)` + quote + `)?\s*\)`)
)

// jsRoutes covers Express, Koa routers, Fastify and Hono: app.get('/users', ...).
func jsRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range jsRouteRe.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: src[m[4]:m[5]], offset: m[0]})
	}
	return out
}

// nestRoutes covers NestJS: @Controller('users') with @Get(':id').
func nestRoutes(src string) []rawRoute {
	prefix := ""
	if m := nestControllerRe.FindStringSubmatch(src); m != nil {
		prefix = m[1]
	}
	var out []rawRoute
	for _, m := range nestRouteRe.FindAllStringSubmatchIndex(src, -1) {
		sub := ""
		if m[4] >= 0 {
			sub = src[m[4]:m[5]]
		}
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: joinURL(prefix, sub), offset: m[0]})
	}
	return out
}

var (
	pyDecoratorRe    = regexp.MustCompile(`@(\w+)\.(get|post|put|patch|delete|head|options)\(\s*[rRuUfF]?['"]([^'"]*)['"]`)
	pyRouteRe        = regexp.MustCompile(`@(\w+)\.(?:route|api_route)\(\s*[rRuUfF]?['"]([^'"]*)['"]([^)]*)\)`)
	pyMethodsRe      = regexp.MustCompile(`methods\s*=\s*[\[\(\{]([^\]\)\}]*)`)
	quotedWordRe     = regexp.MustCompile(`['"](\w+)['"]`)
	pyRouterPrefixRe = regexp.MustCompile(`(\w+)\s*=\s*(?:APIRouter|Blueprint)\(([^)]*)\)`)
	pyPrefixArgRe    = regexp.MustCompile(`(?:url_)?prefix\s*=\s*[rRuUfF]?['"]([^'"]*)['"]`)
	djangoPathRe     = regexp.MustCompile(`\bpath\(\s*[rR]?['"]([^'"]*)['"]`)
)

// pythonRoutes covers FastAPI (@app.get) and Flask (@app.route, methods=[...]),
// including APIRouter(prefix=...) and Blueprint(url_prefix=...).
func pythonRoutes(src string) []rawRoute {
	prefixes := map[string]string{}
	for _, m := range pyRouterPrefixRe.FindAllStringSubmatch(src, -1) {
		if p := pyPrefixArgRe.FindStringSubmatch(m[2]); p != nil {
			prefixes[m[1]] = p[1]
		}
	}
	var out []rawRoute
	for _, m := range pyDecoratorRe.FindAllStringSubmatchIndex(src, -1) {
		recv := src[m[2]:m[3]]
		out = append(out, rawRoute{method: src[m[4]:m[5]], path: joinURL(prefixes[recv], src[m[6]:m[7]]), offset: m[0]})
	}
	for _, m := range pyRouteRe.FindAllStringSubmatchIndex(src, -1) {
		recv, path, args := src[m[2]:m[3]], src[m[4]:m[5]], src[m[6]:m[7]]
		methods := []string{"GET"}
		if mm := pyMethodsRe.FindStringSubmatch(args); mm != nil {
			methods = nil
			for _, w := range quotedWordRe.FindAllStringSubmatch(mm[1], -1) {
				methods = append(methods, w[1])
			}
		}
		for _, method := range methods {
			out = append(out, rawRoute{method: method, path: joinURL(prefixes[recv], path), offset: m[0]})
		}
	}
	return out
}

// djangoRoutes covers path('users/<int:pk>/', ...) in urls.py. Django routes
// accept any method.
func djangoRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range djangoPathRe.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, rawRoute{path: src[m[2]:m[3]], offset: m[0]})
	}
	return out
}

var (
	goHandleRe    = regexp.MustCompile(`(?m)\.(?:HandleFunc|Handle)\(\s*"([^"]+)"[^\n]*?(?:\.Methods\(([^)]*)\)|$)`)
	goFrameworkRe = regexp.MustCompile(`\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any|Get|Post|Put|Patch|Delete|Head|Options)\(\s*"(/[^"]*)"`)
	goQuotedRe    = regexp.MustCompile(`"(\w+)"`)
)

// goRoutes covers net/http (including Go 1.22 "GET /users/{id}" patterns),
// gorilla/mux (.Methods), gin, echo, chi and fiber.
func goRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range goHandleRe.FindAllStringSubmatchIndex(src, -1) {
		pattern := src[m[2]:m[3]]
		method := ""
		if sp := strings.IndexByte(pattern, ' '); sp > 0 && !strings.HasPrefix(pattern, "/") {
			method, pattern = pattern[:sp], strings.TrimSpace(pattern[sp+1:])
		}
		if !strings.HasPrefix(pattern, "/") {
			continue // host-specific pattern
		}
		if m[4] >= 0 && method == "" {
			for _, w := range goQuotedRe.FindAllStringSubmatch(src[m[4]:m[5]], -1) {
				out = append(out, rawRoute{method: w[1], path: pattern, offset: m[0]})
			}
			continue
		}
		out = append(out, rawRoute{method: method, path: pattern, offset: m[0]})
	}
	for _, m := range goFrameworkRe.FindAllStringSubmatchIndex(src, -1) {
		if m[0] >= 3 && src[m[0]-3:m[0]] == "R()" {
			continue // resty client call, not a route
		}
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: src[m[4]:m[5]], offset: m[0]})
	}
	return out
}

var (
	springMappingRe   = regexp.MustCompile(`@(Get|Post|Put|Patch|Delete|Request)Mapping\b(?:\s*\(([^)]*)\))?`)
	springNamedPathRe = regexp.MustCompile(`(?:value|path)\s*=\s*[\{\[]?\s*"([^"]*)"`)
	springMethodArgRe = regexp.MustCompile(`RequestMethod\.(\w+)`)
	classDeclRe       = regexp.MustCompile(`\b(?:class|interface|object)\s+\w+`)
)

// springRoutes covers Spring MVC/WebFlux annotations in Java and Kotlin,
// with a class-level @RequestMapping prefix.
func springRoutes(src string) []rawRoute {
	classPos := len(src)
	if loc := classDeclRe.FindStringIndex(src); loc != nil {
		classPos = loc[0]
	}
	prefix := ""
	var out []rawRoute
	for _, m := range springMappingRe.FindAllStringSubmatchIndex(src, -1) {
		kind := src[m[2]:m[3]]
		args := ""
		if m[4] >= 0 {
			args = src[m[4]:m[5]]
		}
		path := springPath(args)
		if kind == "Request" {
			if m[0] < classPos {
				prefix = path
				continue
			}
			methods := springMethodArgRe.FindAllStringSubmatch(args, -1)
			if len(methods) == 0 {
				out = append(out, rawRoute{path: joinURL(prefix, path), offset: m[0]})
			}
			for _, mm := range methods {
				out = append(out, rawRoute{method: mm[1], path: joinURL(prefix, path), offset: m[0]})
			}
			continue
		}
		out = append(out, rawRoute{method: kind, path: joinURL(prefix, path), offset: m[0]})
	}
	return out
}

func springPath(args string) string {
	if m := springNamedPathRe.FindStringSubmatch(args); m != nil {
		return m[1]
	}
	t := strings.TrimLeft(strings.TrimSpace(args), "{[ ")
	if strings.HasPrefix(t, `"`) {
		if end := strings.IndexByte(t[1:], '"'); end >= 0 {
			return t[1 : 1+end]
		}
	}
	return ""
}

var (
	railsVerbRe      = regexp.MustCompile(`(?m)^\s*(get|post|put|patch|delete)\s+['"]([^'"]+)['"]`)
	railsResourcesRe = regexp.MustCompile(`(?m)^\s*(resources|resource)\s+:(\w+)`)
)

// railsRoutes covers config/routes.rb: verbs and resources/resource.
func railsRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range railsVerbRe.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: src[m[4]:m[5]], offset: m[0]})
	}
	for _, m := range railsResourcesRe.FindAllStringSubmatchIndex(src, -1) {
		name, off := src[m[4]:m[5]], m[0]
		base := "/" + name
		if src[m[2]:m[3]] == "resources" {
			out = append(out,
				rawRoute{"GET", base, off}, rawRoute{"POST", base, off},
				rawRoute{"GET", base + "/:id", off}, rawRoute{"PATCH", base + "/:id", off},
				rawRoute{"PUT", base + "/:id", off}, rawRoute{"DELETE", base + "/:id", off})
		} else {
			out = append(out,
				rawRoute{"GET", base, off}, rawRoute{"POST", base, off}, rawRoute{"PATCH", base, off},
				rawRoute{"PUT", base, off}, rawRoute{"DELETE", base, off})
		}
	}
	return out
}

var (
	axumRouteRe  = regexp.MustCompile(`\.route\(\s*"([^"]+)"\s*,`)
	axumMethodRe = regexp.MustCompile(`\b(get|post|put|patch|delete|head|options|any)\s*\(`)
	rustAttrRe   = regexp.MustCompile(`#\[(get|post|put|patch|delete|head|options)\(\s*"([^"]+)"`)
)

// rustRoutes covers axum (.route("/x", get(h).post(h))) and actix-web/Rocket
// attributes (#[get("/x")]).
func rustRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range axumRouteRe.FindAllStringSubmatchIndex(src, -1) {
		path := src[m[2]:m[3]]
		for _, mm := range axumMethodRe.FindAllStringSubmatch(src[m[1]:callEnd(src, m[1])], -1) {
			method := mm[1]
			if method == "any" {
				method = ""
			}
			out = append(out, rawRoute{method: method, path: path, offset: m[0]})
		}
	}
	for _, m := range rustAttrRe.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: src[m[4]:m[5]], offset: m[0]})
	}
	return out
}

// callEnd returns the index of the parenthesis that closes the call whose
// arguments start at from (we are already one level deep).
func callEnd(src string, from int) int {
	depth := 1
	for i := from; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(src)
}

var laravelRe = regexp.MustCompile(`Route::(get|post|put|patch|delete|options|any)\(\s*['"]([^'"]*)['"]`)

func laravelRoutes(src string) []rawRoute {
	var out []rawRoute
	for _, m := range laravelRe.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, rawRoute{method: src[m[2]:m[3]], path: src[m[4]:m[5]], offset: m[0]})
	}
	return out
}

var (
	angleParamRe = regexp.MustCompile(`<(?:(\w+):)?(\w+)>`)
	braceParamRe = regexp.MustCompile(`\{(\w+)(?:\.\.\.|\?|:([^}]*))?\}`)
)

// normalizePath rewrites framework-specific parameter syntax (:id, <int:id>,
// {id...}, *rest) into OpenAPI style ({id}) and remembers declared types.
func normalizePath(p string) (string, map[string]string) {
	types := map[string]string{}
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "{$}", "")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = angleParamRe.ReplaceAllStringFunc(p, func(m string) string {
		sm := angleParamRe.FindStringSubmatch(m)
		switch sm[1] {
		case "int":
			types[sm[2]] = "integer"
		case "float":
			types[sm[2]] = "number"
		case "uuid":
			types[sm[2]] = "uuid"
		}
		return "{" + sm[2] + "}"
	})
	p = braceParamRe.ReplaceAllStringFunc(p, func(m string) string {
		sm := braceParamRe.FindStringSubmatch(m)
		if re := sm[2]; strings.Contains(re, `\d`) || strings.Contains(re, "[0-9]") || re == "int" {
			types[sm[1]] = "integer"
		}
		return "{" + sm[1] + "}"
	})
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, ":") && len(s) > 1:
			name := strings.TrimSuffix(s[1:], "?")
			if idx := strings.IndexByte(name, '('); idx >= 0 {
				if strings.Contains(name[idx:], `\d`) {
					types[name[:idx]] = "integer"
				}
				name = name[:idx]
			}
			segs[i] = "{" + name + "}"
		case strings.HasPrefix(s, "*"):
			name := s[1:]
			if name == "" {
				name = "wildcard"
			}
			segs[i] = "{" + name + "}"
		}
	}
	return strings.Join(segs, "/"), types
}

var tmplParamRe = regexp.MustCompile(`\{([^}/]+)\}`)

// PathParamNames lists the {name} parameters of a path template in order.
func PathParamNames(path string) []string {
	var out []string
	for _, m := range tmplParamRe.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

func routeEndpoint(r route) *model.Endpoint {
	ep := &model.Endpoint{Method: r.method, Path: r.path, Source: r.where, Code: r.where, Inferred: true}
	if ep.Method == "" {
		ep.Method, ep.AnyMethod = "GET", true
	}
	for _, name := range PathParamNames(r.path) {
		prm := model.Param{Name: name, In: "path", Required: true}
		switch r.types[name] {
		case "integer":
			prm.Schema = &model.Schema{Type: "integer"}
		case "number":
			prm.Schema = &model.Schema{Type: "number"}
		case "uuid":
			prm.Schema = &model.Schema{Type: "string", Format: "uuid"}
		}
		ep.Params = append(ep.Params, prm)
	}
	switch ep.Method {
	case "POST", "PUT", "PATCH":
		ep.BodyType = "application/json" // the handler's body shape is unknown
	}
	return ep
}

// servesProtocol reports whether a route in the code is the mount point of a
// GraphQL or WebSocket endpoint, not an HTTP endpoint of its own.
func servesProtocol(api *model.API, r route) bool {
	for _, ep := range api.Endpoints {
		if (ep.Protocol == model.ProtoGraphQL || ep.Protocol == model.ProtoWebSocket) && pathsMatch(ep.Path, r.path) {
			return true
		}
	}
	return false
}

// computeDrift compares the spec's endpoints with the routes in the code.
// Only HTTP endpoints take part: gRPC methods have no routes, and a GraphQL
// or WebSocket endpoint shares one route.
func computeDrift(api *model.API, routes []route) {
	var specEndpoints []*model.Endpoint
	for _, ep := range api.Endpoints {
		if ep.Protocol == "" {
			specEndpoints = append(specEndpoints, ep)
		}
	}
	for _, r := range routes {
		if servesProtocol(api, r) {
			continue
		}
		matched := false
		for _, ep := range specEndpoints {
			if (r.method == "" || r.method == ep.Method) && pathsMatch(ep.Path, r.path) {
				matched = true
				break
			}
		}
		if matched || isSpecRoute(r.path) {
			continue
		}
		method := r.method
		if method == "" {
			method = "ANY"
		}
		api.Drift = append(api.Drift, model.Drift{Kind: "undocumented", Method: method, Path: r.path, Where: r.where})
		ep := routeEndpoint(r)
		if api.Endpoint(ep.Key()) == nil {
			api.Endpoints = append(api.Endpoints, ep)
		}
	}
	for _, ep := range specEndpoints {
		found := false
		for _, r := range routes {
			if (r.method == "" || r.method == ep.Method) && pathsMatch(ep.Path, r.path) {
				found = true
				if ep.Code == "" {
					ep.Code = r.where
				}
				break
			}
		}
		// A handler the agent located in the code is not drift, even when the
		// patterns missed its route.
		if !found && ep.Code == "" {
			api.Drift = append(api.Drift, model.Drift{Kind: "unimplemented", Method: ep.Method, Path: ep.Path, Where: ep.Source})
		}
	}
}

// isSpecRoute reports routes that serve the API document itself.
func isSpecRoute(path string) bool {
	for _, p := range probePaths {
		if path == p {
			return true
		}
	}
	lower := strings.ToLower(path)
	return strings.HasPrefix(lower, "/docs") || strings.HasPrefix(lower, "/swagger") || strings.HasPrefix(lower, "/redoc")
}

// pathsMatch compares templates ignoring parameter names and trailing slashes.
// A route mounted under a prefix (/users in code, /api/users in the spec)
// still matches.
func pathsMatch(a, b string) bool {
	ca, cb := canonical(a), canonical(b)
	if ca == cb {
		return true
	}
	if len(ca) < len(cb) {
		ca, cb = cb, ca
	}
	return cb != "" && strings.HasSuffix(ca, cb) && strings.HasPrefix(cb, "/")
}

func canonical(p string) string {
	return strings.TrimRight(tmplParamRe.ReplaceAllString(p, "{}"), "/")
}

func joinURL(prefix, path string) string {
	prefix = strings.Trim(prefix, "/")
	path = strings.TrimLeft(path, "/")
	switch {
	case prefix == "":
		return "/" + path
	case path == "":
		return "/" + prefix
	}
	return "/" + prefix + "/" + path
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
