package discover

import (
	"sort"
	"strings"
	"testing"
)

const oas3 = `
openapi: 3.1.0
info: {title: Shop}
servers:
  - url: https://api.example.com/{version}
    variables:
      version: {default: v2}
security:
  - key: []
paths:
  /items/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: integer}}
    get:
      security: []
      responses:
        200:
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Item'}
    put:
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Item'}
            example: {name: lamp, price: 3}
      responses:
        "2XX": {description: ok}
components:
  securitySchemes:
    key: {type: apiKey, in: header, name: X-Shop-Key}
  schemas:
    Base:
      type: object
      required: [name]
      properties:
        name: {type: string, maxLength: 20}
    Item:
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          properties:
            price: {type: [number, "null"], exclusiveMinimum: 0}
            parent: {$ref: '#/components/schemas/Item'}
`

func TestParseOpenAPI3(t *testing.T) {
	api, err := ParseSpec([]byte(oas3), "spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if api.Title != "Shop" || len(api.Endpoints) != 2 {
		t.Fatalf("got title %q and %d endpoints", api.Title, len(api.Endpoints))
	}
	get, put := api.Endpoint("GET /v2/items/{id}"), api.Endpoint("PUT /v2/items/{id}")
	if get == nil || put == nil {
		t.Fatalf("endpoints not found under the server base path: %+v", api.Endpoints)
	}
	if get.Secured || !put.Secured {
		t.Errorf("security: GET secured=%v (want false), PUT secured=%v (want true)", get.Secured, put.Secured)
	}
	if len(api.AuthHeaders) != 1 || api.AuthHeaders[0] != "X-Shop-Key" {
		t.Errorf("auth headers = %v", api.AuthHeaders)
	}
	if p := get.Params; len(p) != 1 || p[0].Schema.Type != "integer" || !p[0].Required {
		t.Errorf("path params = %+v", p)
	}
	item := get.Responses["200"].Schema
	if item == nil || !item.IsRequired("name") || item.Properties["name"].MaxLength == nil {
		t.Fatalf("allOf was not merged: %+v", item)
	}
	price := item.Properties["price"]
	if price.Type != "number" || !price.Nullable || !price.ExclusiveMinimum || *price.Minimum != 0 {
		t.Errorf("price = %+v", price)
	}
	if item.Properties["parent"] == nil {
		t.Error("recursive property was dropped")
	}
	if !put.BodyRequired || put.Body.Example == nil {
		t.Errorf("request body: required=%v example=%v", put.BodyRequired, put.Body.Example)
	}
	if get.Responses["200"].Schema.Example != nil {
		t.Error("the request body example leaked into the shared schema")
	}
	if _, ok := put.Responses["2XX"]; !ok {
		t.Errorf("responses = %v", put.Responses)
	}
}

const swagger2 = `{
  "swagger": "2.0",
  "basePath": "/api/",
  "paths": {
    "/login": {
      "post": {
        "consumes": ["application/x-www-form-urlencoded"],
        "parameters": [
          {"name": "user", "in": "formData", "type": "string", "required": true},
          {"name": "remember", "in": "formData", "type": "boolean"}
        ],
        "responses": {"200": {"description": "ok", "schema": {"type": "object"}}}
      }
    },
    "/pets": {
      "post": {
        "parameters": [{"name": "pet", "in": "body", "required": true, "schema": {"$ref": "#/definitions/Pet"}}],
        "responses": {"201": {"description": "created"}}
      },
      "get": {
        "parameters": [{"name": "limit", "in": "query", "type": "integer", "maximum": 50}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  },
  "definitions": {"Pet": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}}
}`

func TestParseSwagger2(t *testing.T) {
	api, err := ParseSpec([]byte(swagger2), "swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	login := api.Endpoint("POST /api/login")
	if login == nil || login.BodyType != "application/x-www-form-urlencoded" || !login.Body.IsRequired("user") {
		t.Fatalf("form body not parsed: %+v", login)
	}
	pets := api.Endpoint("POST /api/pets")
	if pets == nil || pets.Body == nil || !pets.Body.IsRequired("name") || !pets.BodyRequired {
		t.Fatalf("body parameter not parsed: %+v", pets)
	}
	list := api.Endpoint("GET /api/pets")
	if list == nil || len(list.Params) != 1 || list.Params[0].Schema.Type != "integer" || *list.Params[0].Schema.Maximum != 50 {
		t.Fatalf("swagger 2 parameter schema not read from the parameter: %+v", list)
	}
}

func TestParseSpecRejectsOtherDocuments(t *testing.T) {
	if _, err := ParseSpec([]byte(`{"name": "package.json"}`), "package.json"); err == nil {
		t.Fatal("want an error for a non-OpenAPI document")
	}
}

func routeKeys(rs []route) []string {
	var out []string
	for _, r := range rs {
		m := r.method
		if m == "" {
			m = "ANY"
		}
		out = append(out, m+" "+r.path)
	}
	sort.Strings(out)
	return out
}

func TestExtractRoutes(t *testing.T) {
	cases := []struct {
		file, src string
		want      []string
	}{
		{"server.js", "app.get('/users/:id', h)\nrouter.post(`/users`, h)\nuserRouter.delete(\"/users/:id(\\\\d+)\", h)\naxios.get('/not-a-route')",
			[]string{"DELETE /users/{id}", "GET /users/{id}", "POST /users"}},
		{"cats.controller.ts", "@Controller('cats')\nclass C {\n @Get(':id') one() {}\n @Post() make() {}\n}",
			[]string{"GET /cats/{id}", "POST /cats"}},
		{"main.py", "router = APIRouter(prefix=\"/v1\")\n@router.get(\"/items/{item_id}\")\n@app.route('/login', methods=['GET', 'POST'])\n@bp.route('/plain')",
			[]string{"GET /login", "GET /plain", "GET /v1/items/{item_id}", "POST /login"}},
		{"urls.py", "urlpatterns = [path('articles/<int:year>/', v)]", []string{"ANY /articles/{year}/"}},
		{"main.go", "mux.HandleFunc(\"GET /users/{id}\", h)\nhttp.Handle(\"/static/\", fs)\nr.HandleFunc(\"/orders\", h).Methods(\"POST\", \"PUT\")\ne.GET(\"/ping\", h)\nclient.R().Get(\"/remote\")",
			[]string{"ANY /static/", "GET /ping", "GET /users/{id}", "POST /orders", "PUT /orders"}},
		{"UserController.java", "@RequestMapping(\"/api/users\")\npublic class UserController {\n @GetMapping(\"/{id}\") User one() {}\n @PostMapping User make() {}\n @RequestMapping(value = \"/bulk\", method = RequestMethod.PUT) void bulk() {}\n}",
			[]string{"GET /api/users/{id}", "POST /api/users", "PUT /api/users/bulk"}},
		{"config/routes.rb", "Rails.application.routes.draw do\n  resources :photos\n  get 'about', to: 'pages#about'\nend",
			[]string{"DELETE /photos/{id}", "GET /about", "GET /photos", "GET /photos/{id}", "PATCH /photos/{id}", "POST /photos", "PUT /photos/{id}"}},
		{"main.rs", "Router::new()\n  .route(\"/users/:id\", get(show).delete(remove))\n  .route(\"/health\", get_service(svc))\n#[post(\"/items\")]",
			[]string{"DELETE /users/{id}", "GET /users/{id}", "POST /items"}},
		{"routes/api.php", "Route::get('users/{id?}', [C::class, 'show']);", []string{"GET /users/{id}"}},
	}
	for _, c := range cases {
		got := routeKeys(extractRoutes(c.file, c.src))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s:\n got  %v\n want %v", c.file, got, c.want)
		}
	}
}

func TestNormalizePathTypes(t *testing.T) {
	cases := []struct{ in, want, param, typ string }{
		{"/users/<int:id>", "/users/{id}", "id", "integer"},
		{"/files/{path...}", "/files/{path}", "", ""},
		{"/a/{id:[0-9]+}", "/a/{id}", "id", "integer"},
		{"/static/*filepath", "/static/{filepath}", "", ""},
		{"items/:id?", "/items/{id}", "", ""},
		{"/exact/{$}", "/exact/", "", ""},
	}
	for _, c := range cases {
		got, types := normalizePath(c.in)
		if got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.param != "" && types[c.param] != c.typ {
			t.Errorf("normalizePath(%q) type of %s = %q, want %q", c.in, c.param, types[c.param], c.typ)
		}
	}
}

func TestPathsMatch(t *testing.T) {
	if !pathsMatch("/api/users/{id}", "/users/{userId}") {
		t.Error("a route mounted under a prefix should match")
	}
	if !pathsMatch("/users/", "/users") {
		t.Error("trailing slashes should not matter")
	}
	if pathsMatch("/users/{id}", "/orders/{id}") || pathsMatch("/xusers", "/users") {
		t.Error("different paths matched")
	}
}

func TestTemplatize(t *testing.T) {
	got, values := Templatize("/users/42/orders/3fa85f64-5717-4562-b3fc-2c963f66afa6/items")
	if got != "/users/{userId}/orders/{orderId}/items" {
		t.Fatalf("got %q", got)
	}
	if values["userId"] != "42" || values["orderId"] == "" {
		t.Errorf("values = %v", values)
	}
	if got, _ := Templatize("/categories/7/categories/8"); got != "/categories/{categoryId}/categories/{categoryId2}" {
		t.Errorf("repeated names: got %q", got)
	}
}

func TestEndpointsFromTraffic(t *testing.T) {
	exs := []Exchange{
		{Request: req("POST", "/users", "", `{"name":"a","age":3,"tags":["x"]}`)},
		{Request: req("POST", "/users", "", `{"name":"b","age":4.5,"nick":null}`)},
		{Request: req("GET", "/users/1", "fields=name&page=2", "")},
		{Request: req("GET", "/users/2", "page=3", "")},
	}
	api, keys := EndpointsFromTraffic(exs)
	if len(api.Endpoints) != 2 || keys[2] != "GET /users/{userId}" {
		t.Fatalf("endpoints = %d, keys = %v", len(api.Endpoints), keys)
	}
	post := api.Endpoint("POST /users")
	if post.Body.Properties["age"].Type != "number" {
		t.Errorf("age should widen to number, got %q", post.Body.Properties["age"].Type)
	}
	if !post.Body.IsRequired("name") || post.Body.IsRequired("tags") {
		t.Errorf("required = %v", post.Body.Required)
	}
	get := api.Endpoint("GET /users/{userId}")
	var page, fields bool
	for _, p := range get.Params {
		switch p.Name {
		case "page":
			page = p.Required && p.Schema.Type == "integer"
		case "fields":
			fields = !p.Required
		}
	}
	if !page || !fields {
		t.Errorf("query params = %+v", get.Params)
	}
}
