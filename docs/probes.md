# Built-in probes, checks and traffic modes

The `probe` tool and `replayharness run` generate requests from the endpoints
that discovery finds. The replay agent uses them as a fast baseline before it writes replays.
They also work on their own, in CI or by hand.

## Sources

| Source | Example | What is used |
|---|---|---|
| Source tree | `replayharness run . --target URL` | The API map in `.replayharness/api.yaml`, OpenAPI/Swagger files in the tree, and routes in the code. A route that the map and the spec both miss is reported as drift and probed anyway. With neither, only the routes found in the code are used. |
| API map | `replayharness run .replayharness/api.yaml --target URL` | The endpoints, example bodies, required fields, limits and auth that the discovery agent wrote |
| Running server | `replayharness run http://localhost:8000` | The spec served at `/openapi.json` (FastAPI), `/v3/api-docs` (Spring), `/swagger.json`, `/swagger/v1/swagger.json` (ASP.NET) and similar places |
| Spec file | `replayharness run openapi.yaml --target URL` | OpenAPI 3.x or Swagger 2.0, JSON or YAML |
| Recorded traffic | `replayharness run traffic.jsonl --target URL` | Endpoints, path templates, query parameters and body shapes inferred from real requests (also `.har`) |
| Scenarios file | `replayharness run scenarios.json --target URL` | Probes written earlier by `replayharness gen` |
| `.proto` files | `replayharness run . --target URL --grpc host:port` | gRPC services and their messages, with `field_behavior` REQUIRED fields, oneofs and enums. `--proto` adds files or directories outside the tree. Vendored protos (`vendor/`, `third_party/`) are used for imports only. |
| gRPC reflection | `replayharness run grpc://localhost:9090` | Services the server lists by reflection (v1, or v1alpha), when there are no `.proto` files |
| GraphQL schema | `replayharness run . --target URL` | Queries and mutations from `.graphql`/`.graphqls`/`.gql` files and `gql` literals in JS/TS, or from introspection of the running server. Each root field is one endpoint. |

Routes are found in Go (net/http incl. Go 1.22 patterns, gorilla/mux, gin,
echo, chi, fiber), JavaScript/TypeScript (Express, Koa routers, Fastify, Hono,
NestJS), Python (FastAPI incl. `APIRouter(prefix=…)`, Flask incl. blueprints,
Django `urls.py`), Java/Kotlin (Spring), Rust (axum, actix-web, Rocket), Ruby
(Rails `routes.rb`) and PHP (Laravel).

For routes found only by pattern or in traffic, nothing says what valid
input is. So guessed rules, such as a field that was always present or a
method that was never seen, only count when the server crashes or returns a
5xx. An API map fixes that: its required fields and limits count like a
spec's.

## Probe categories

For each endpoint there is one valid request, built from the spec's examples
and defaults where there are any, and then variants that each break one thing:

| Category | Examples | A good server… |
|---|---|---|
| `happy` | the valid request | accepts it (2xx) |
| `auth` | no credentials, an invalid bearer token (endpoints the spec marks as secured) | rejects it (401/403) |
| `missing-required` | each required body field, query parameter or header left out, nested fields too | rejects it (4xx) |
| `wrong-type` | `"abc"` or `1.5` for an integer, `12345` for a string, `{}` for an array | rejects it |
| `null` | `null` for each non-nullable field | rejects it, or ignores an optional one |
| `boundary` | exactly `minimum`/`maximum`/`minLength`/`maxLength`, then one past; a value not in the `enum`; an enum value in the wrong case | accepts the limits, rejects one past |
| `format` | `"not-an-email"`, `"not-a-uuid"`, `"2024-13-45"` | rejects it |
| `extreme` | int64 max + 1, `1e400`, empty and whitespace-only strings, NUL and control characters, emoji/CJK/RTL text, a 10 000-char string, 1 000-item arrays | doesn't crash |
| `body-shape` | empty body, `{}`, `[]`, `null`, truncated JSON, a trailing comma, JSON nested 1 000 levels deep, duplicate keys, an unknown field, a 1 MiB body, a wrong or missing `Content-Type`, a byte-order mark | rejects or handles it, doesn't crash |
| `path-param` | `0`, `-1`, `999999999`, a 20-digit number, `abc`, `1.5`, an encoded slash, a space, unicode, 2 000 characters | answers 4xx, doesn't crash |
| `query` | each optional parameter, empty values, a parameter given twice, unknown parameters, pagination guesses (`limit=-1`, `page=0`) for routes found in code | doesn't crash |
| `headers` | `Accept: application/xml`, required headers missing, a non-numeric value for a numeric header | doesn't crash |
| `method` | every method the path doesn't define | answers 405 (or 404) |
| `consistency` | the same GET twice | answers the same way twice |
| `replay` | recorded requests | answers like it did when recorded |

### Other protocols

* **gRPC:** the same body categories apply to the request message, minus
  wrong types protobuf's JSON parser rejects before sending (those are
  dropped), plus an enum number that is not defined and an empty message.
  A code counts as its HTTP equivalent: `INVALID_ARGUMENT` is a 400,
  `NOT_FOUND` a 404, `INTERNAL`, `UNKNOWN` and `DATA_LOSS` a 500,
  `UNAVAILABLE` a 503.
* **GraphQL:** the body categories apply to the variables. `body-shape`
  sends `{}`, a query that is a number, malformed JSON, a query that does
  not parse, an unknown field, an unknown `operationName`, a batch, and a
  `text/plain` body; `{ __typename }` must succeed. `method` sends each
  mutation with GET, which must not run.
* **WebSocket:** the body categories apply to the first message, plus
  malformed JSON, an empty message, plain text, an unknown message type
  and a message of 1 MiB. Only a crash counts: a close with 1011, or a
  connection dropped without a close frame. The handshake is checked like
  a GET, including without credentials.

To narrow a run:
* `--only` and `--skip` choose categories.
* `--endpoint` filters endpoints by a regular expression.
* `--max-per-endpoint` caps the probes per endpoint (default 250).
* `--dry-run` lists the probes without sending them.
* `replayharness gen` writes them to a file you can edit and run later.

## Checks

| Check | Severity | Meaning |
|---|---|---|
| `server-error` | error | a 5xx response, gRPC `INTERNAL`/`UNKNOWN`/`DATA_LOSS`, a GraphQL error that looks like a crash, or a WebSocket closed with 1011 |
| `connection` / `timeout` | error | the server dropped the connection or never answered, usually a crash |
| `auth-bypass` | error | a secured endpoint answered 2xx without valid credentials |
| `accepted-invalid` | error or warning | invalid input got a 2xx. It is an error when the spec's own rule was broken (a missing required field, a value outside the enum), otherwise a warning. |
| `schema-mismatch` | error | a 2xx body doesn't match the documented schema (types, required fields, enums) |
| `regression` | error or warning | replay: the status changed (error) or the JSON shape changed (warning) |
| `divergence` | error or warning | the baseline server answered differently |
| `undocumented-status` | warning | a status code the spec doesn't list for the endpoint |
| `error-leak` | warning | a stack trace or database error in the body (Python, Go, JVM, Node, .NET, Ruby, PHP, SQL) |
| `undefined-method` | warning | a method the path doesn't define answered 2xx |
| `get-mutation` | error | a GraphQL mutation sent with GET ran |
| `content-type` | warning | a JSON response served with another content type |
| `slow` | warning | slower than `--slow` (default 2s) |
| `inconsistent` | warning | the same GET answered differently twice |
| `rejected-valid` | warning or note | the valid request was refused. A 401, 403 or 404 is only a note, since it usually means missing credentials or seed data. |

Identical findings on one endpoint are grouped and counted, each with a `curl`
(or `grpcurl`, or `websocat`) command that reproduces it. The exit status is 1 when there are errors.
`--fail-on warning` makes warnings fail the run too, and `--fail-on never`
always exits 0. `--json` writes the full report, and `--junit` writes one test
case per probe.

## Record and replay

```sh
replayharness record --upstream http://localhost:8080 -o traffic.jsonl   # proxy on 127.0.0.1:8099
replayharness replay traffic.jsonl --target http://localhost:8080        # did any response change?
replayharness replay traffic.jsonl --target http://localhost:8080 --mutate   # also break recorded requests one field at a time
```

Put the proxy in front of your server and use your app or run your end-to-end
tests through it. Replay compares each response with the recording: a
different status is an error, and a differently shaped JSON body (keys and
types) is a warning. `--exact` also compares values, except keys that change
between runs anyway (`id`, `createdAt`, `timestamp`, …). Set those keys with
`--ignore`. HAR files from browser dev tools work in place of `traffic.jsonl`.

## Two versions side by side

```sh
replayharness run . --target http://localhost:8080 --baseline http://localhost:9090
```

Every probe goes to both servers. A different status code is an error, and a
differently shaped body is a warning.
