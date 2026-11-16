# ReplayHarness

**Point it at your API (a repo, a spec, a running server, or recorded traffic) and it writes the requests that break it.**

ReplayHarness finds your endpoints, generates hundreds of requests that each
break one rule, sends them, and tells you what went wrong. Each request breaks
one thing: a missing field, a wrong type, a value one past the limit, a
malformed body, no credentials, or a method the path doesn't define. You learn
which requests crashed the server, which invalid ones it accepted, and where
its answers break its own spec. Every finding comes with a `curl` command that
reproduces it.

One Go binary. No config file. Works without a spec.

```text
$ replayharness run ./examples/buggyapi --target http://127.0.0.1:8080

ReplayHarness 0.1.0 · 144 scenarios · 6 endpoints · http://127.0.0.1:8080 · 2.98s

DELETE /users/{id}
  ✗ error   auth-bypass         returned 204 to a request without valid credentials, but the spec requires authentication  ×2
    e.g. no credentials
    curl -sS -i -X DELETE 'http://127.0.0.1:8080/users/1'

GET /search
  ✗ error   connection          connection failed on boundary input (the server may have crashed or dropped the request)  ×2
    e.g. query q is 1 char (minLength)
    curl -sS -i 'http://127.0.0.1:8080/search?q=t'
  ! warning accepted-invalid    accepted boundary input with 200  ×3
    e.g. query q is 101 chars (above maxLength 100)

GET /users/{id}
  ✗ error   schema-mismatch     response does not match the documented schema: $.age is a string, expected an integer  ×5
    e.g. valid request
    curl -sS -i 'http://127.0.0.1:8080/users/1'
  ✗ error   server-error        500 on an unusual path parameter  ×3
    e.g. path id = "0"
    panic: runtime error: index out of range [-1] goroutine 46 [running]: ...
    curl -sS -i 'http://127.0.0.1:8080/users/0'
  ! warning error-leak          response body contains a Go stack trace  ×3

POST /users
  ✗ error   server-error        500 on wrong-type input  ×2
    e.g. body.age = "abc" (string, expected integer)
    curl -sS -i -X POST 'http://127.0.0.1:8080/users' -H 'Content-Type: application/json' --data-raw '{"age":"abc","email":"user@example.com","name":"Test Name"}'
  ✗ error   accepted-invalid    accepted a request missing a required field with 201
    e.g. body.email omitted (required)
  ! warning accepted-invalid    accepted boundary input with 201
    e.g. body.name is 51 chars (above maxLength 50)
  ...

✓ 2 endpoint(s) with no problems: GET /admin/stats, GET /health

Spec drift
  GET     /admin/stats                 in the code, missing from the spec  buggyapi.go:58

10 error(s) · 8 warning(s) · 0 note(s)
```

That is real output (trimmed) from the demo API in [`examples/buggyapi`](examples/buggyapi),
which has one deliberate bug per `BUG` comment. ReplayHarness finds all of them.

## Install

```sh
go install github.com/aryehlev/ReplayHarness/cmd/replayharness@latest
```

Needs Go 1.22 or newer. Or build from a clone with `go build ./cmd/replayharness`.

## Try it in 30 seconds

```sh
git clone https://github.com/aryehlev/ReplayHarness && cd ReplayHarness
go run ./examples/buggyapi/cmd/buggy-api &        # a small API with planted bugs
go run ./cmd/replayharness run ./examples/buggyapi --target http://127.0.0.1:8080
```

## Point it at your service

ReplayHarness takes one source and turns it into scenarios. Pick whichever you have.

### A source tree

```sh
replayharness run . --target http://localhost:8080
```

If the tree has OpenAPI or Swagger files, they are used. Routes in the code are
compared with them: a route the spec doesn't mention is reported as **spec
drift** and tested anyway. With no spec at all, the routes in the code are the
endpoints. They are found in:

| Language | Frameworks |
|---|---|
| Go | net/http (incl. Go 1.22 `"GET /users/{id}"`), gorilla/mux, gin, echo, chi, fiber |
| JavaScript / TypeScript | Express, Koa routers, Fastify, Hono, NestJS |
| Python | FastAPI (incl. `APIRouter(prefix=…)`), Flask (incl. blueprints), Django `urls.py` |
| Java / Kotlin | Spring (`@GetMapping`, class-level `@RequestMapping`) |
| Rust | axum, actix-web, Rocket |
| Ruby | Rails `routes.rb` (verbs and `resources`) |
| PHP | Laravel |

Without a spec, nobody has said what valid input looks like, so ReplayHarness
only reports what is wrong for sure: crashes, 5xx errors, dropped connections,
timeouts and leaked stack traces.

### A running server

```sh
replayharness run http://localhost:8000
```

It fetches the server's OpenAPI document from where frameworks usually serve it:
`/openapi.json` (FastAPI), `/v3/api-docs` (Spring), `/swagger.json`,
`/swagger/v1/swagger.json` (ASP.NET) and more. You can also pass the
document's URL or a local spec file.

### Real traffic: record, then replay

```sh
# 1. Put the proxy in front of your server, then use your app or run your e2e tests through it.
replayharness record --upstream http://localhost:8080 -o traffic.jsonl     # listens on 127.0.0.1:8099

# 2. After a change, replay it. Any response that changed status or shape is a regression.
replayharness replay traffic.jsonl --target http://localhost:8080

# 3. Mutate the recorded requests too: real ids and real bodies, one thing broken at a time.
replayharness replay traffic.jsonl --target http://localhost:8080 --mutate
```

Endpoints, path templates (`/users/42` becomes `/users/{userId}`), query
parameters and JSON body shapes are inferred from the traffic. HAR files exported
from browser dev tools or a proxy work in place of `traffic.jsonl`. Credentials
(`Authorization`, `Cookie`, API-key headers) are left out of recordings unless
you pass `--keep-secrets`. Replay compares the shape of JSON bodies (keys and
types) by default. `--exact` compares values too, except keys that usually
change between runs (`id`, `createdAt`, `timestamp`, …; set them with `--ignore`).

### Two versions side by side

```sh
replayharness run . --target http://localhost:8080 --baseline http://localhost:9090
```

Every scenario goes to both servers. A different status code is an error; a
differently shaped JSON body is a warning. This catches behavior changes that
your spec doesn't describe.

## What it sends

For each endpoint it builds one valid request, using the spec's examples and
defaults where there are any. Then it sends variants that each break one thing:

| Category | Examples | A good server… |
|---|---|---|
| `happy` | the valid request | accepts it (2xx) |
| `auth` | no credentials, an invalid bearer token (endpoints the spec marks as secured) | rejects it (401/403) |
| `missing-required` | each required body field, query parameter or header left out, nested fields included | rejects it (4xx) |
| `wrong-type` | `"abc"` for an integer, `1.5` for an integer, `12345` for a string, `{}` for an array | rejects it |
| `null` | `null` for each non-nullable field | rejects it, or ignores an optional one |
| `boundary` | exactly `minimum`/`maximum`/`minLength`/`maxLength`, then one past; a value not in the `enum`, an enum value in the wrong case | accepts the limits, rejects one past |
| `format` | `"not-an-email"`, `"not-a-uuid"`, `"2024-13-45"` | rejects it |
| `extreme` | int64 max + 1, `1e400`, empty and whitespace-only strings, NUL and control characters, emoji/CJK/RTL text, a 10 000-char string, 1 000-item arrays | doesn't crash |
| `body-shape` | empty body, `{}`, `[]`, `null`, truncated JSON, a trailing comma, JSON nested 1 000 levels deep, duplicate keys, an unknown field, a 1 MiB body, the wrong or a missing `Content-Type`, a byte-order mark | rejects or handles it, doesn't crash |
| `path-param` | `0`, `-1`, `999999999`, a 20-digit number, `abc`, `1.5`, an encoded slash, a space, unicode, 2 000 characters | answers 4xx, doesn't crash |
| `query` | each optional parameter, empty values, a parameter given twice, unknown parameters, pagination guesses (`limit=-1`, `page=0`) for routes found in code | doesn't crash |
| `headers` | `Accept: application/xml`, required headers missing, a non-numeric value for a numeric header | doesn't crash |
| `method` | every method the path doesn't define | answers 405 (or 404) |
| `consistency` | the same GET twice | answers the same way twice |
| `replay` | recorded requests | answers like it did when recorded |

`--only` and `--skip` choose categories, `--endpoint` filters endpoints by regex,
`--dry-run` lists the scenarios without sending them, and `replayharness gen`
writes them to a JSON file you can edit, commit and run later with
`replayharness run scenarios.json`.

## What it reports

| Check | Severity | Meaning |
|---|---|---|
| `server-error` | error | a 5xx response |
| `connection` / `timeout` | error | the server dropped the connection or never answered: usually a crash |
| `auth-bypass` | error | a secured endpoint answered 2xx without valid credentials |
| `accepted-invalid` | error / warning | invalid input got a 2xx. It is an error when the spec's own rule was broken (a missing required field, a value outside the enum), otherwise a warning |
| `schema-mismatch` | error | a 2xx response body doesn't match the documented schema (types, required fields, enums) |
| `regression` | error / warning | replay: the status changed (error) or the JSON shape changed (warning) |
| `divergence` | error / warning | the baseline server answered differently |
| `undocumented-status` | warning | a status code the spec doesn't list for the endpoint |
| `error-leak` | warning | a stack trace or database error in the body (Python, Go, JVM, Node, .NET, Ruby, PHP, SQL) |
| `undefined-method` | warning | a method the path doesn't define answered 2xx |
| `content-type` | warning | a JSON response served with another content type |
| `slow` | warning | slower than `--slow` (default 2s) |
| `inconsistent` | warning | the same GET answered differently twice |
| `rejected-valid` | warning / note | the valid request was refused. A 401, 403 or 404 is only a note, since it usually means missing credentials or seed data |

Identical findings on one endpoint are grouped and counted, so 40 scenarios that
hit the same crash appear as one line. The exit status is 1 when there are
errors (`--fail-on warning` tightens that, `--fail-on never` turns it off).
`--json report.json` writes everything, including every request and response (bodies trimmed to 4 KB).
`--junit report.xml` writes one test case per scenario for CI.

## In CI

```yaml
# .github/workflows/api-check.yml
jobs:
  replayharness:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - run: docker compose up -d --wait            # start your service and its database
      - run: go install github.com/aryehlev/ReplayHarness/cmd/replayharness@latest
      - run: replayharness run . --target http://localhost:8080 -H "Authorization: Bearer ${{ secrets.TEST_TOKEN }}" --junit replayharness.xml
```

Pass credentials with `-H`. They are sent with every request except the
scenarios that test missing credentials, and they are redacted in printed
`curl` commands.

## Safety

* **Only your own servers.** Generated traffic goes only to `localhost` and
  private addresses (`127.0.0.0/8`, `10.0.0.0/8`, `192.168.0.0/16`, …). Any other
  host is refused unless you pass `--allow-remote`, and you should only do that
  for a host you own.
* **Scenarios write data.** POST, PUT, PATCH and DELETE scenarios create, change
  and delete records, and undefined-method probes can reach handlers the spec
  doesn't mention. Run against a disposable environment with test data, never
  production.
* **Gentle by default.** At most 50 requests per second and 4 in flight
  (`--rate`, `--concurrency`), a 10s timeout, and a cap of 250 scenarios per
  endpoint (`--max-per-endpoint`).

## Limits

* Route extraction from code uses patterns, not a parser. It misses routes built
  at runtime, and router prefixes set in another file (`app.use('/api', router)`)
  are not joined. Drift matching tolerates the missing prefix.
* It understands the common part of JSON Schema: types, required, enum, numeric
  and length limits, formats, nullable, `allOf`/`oneOf`/`anyOf` and local `$ref`s.
  It does not generate strings for a `pattern`, and external `$ref`s are not followed.
* Each scenario is a single request. There are no multi-step sequences yet
  (create a user, then fetch it with the new id); recorded traffic gets closest,
  because it carries real ids.
* REST over HTTP/JSON (and form bodies) only: no GraphQL or gRPC.

## How it compares

[Schemathesis](https://github.com/schemathesis/schemathesis) (property-based
testing from OpenAPI and GraphQL schemas) and
[RESTler](https://github.com/microsoft/restler-fuzzer) (stateful fuzzing from
OpenAPI) are mature and go deeper on spec-driven generation and request
sequences. Use them if you have a complete spec. ReplayHarness is for when
you don't, or when you want more than the spec gives you: it reads routes
straight from the source, records and mutates real traffic, diffs two versions
of a server, reports drift between the code and the spec, and ships as one
binary with no setup.

## Layout

```text
cmd/replayharness     the CLI
internal/discover     OpenAPI/Swagger parsing, route extraction from code, spec fetching, traffic inference
internal/generate     scenario generation
internal/runner       sending requests (rate limit, concurrency, safety check)
internal/check        the checks that turn responses into findings
internal/report       grouping, terminal output, JSON and JUnit
internal/record       the recording proxy
examples/buggyapi     the demo API with planted bugs, used by the end-to-end test
```

`go test ./...` runs everything, including an end-to-end test that runs the full
pipeline against the demo API and fails if any planted bug is missed or the
correct endpoint gets a false positive.

## License

Dual-licensed under [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at your option.
