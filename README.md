# ReplayHarness

**Coding agents that find bugs in your API by sending it real requests.**
REST, gRPC, GraphQL and WebSocket. ReplayHarness works with or without an
OpenAPI spec, a `.proto` file or a GraphQL schema. It uses two agents:

1. **A discovery agent** reads your code and maps the API: every route, its
   handler's `file:line`, its parameters and body, its auth, and the rules
   the handler enforces.
2. **A replay agent** reads that map and the handlers. It writes replays,
   which are multi-step requests that check what each handler should do,
   runs them against your local server, and reports the bugs it confirms.

ReplayHarness gives both agents the tools: route discovery, a safe client
fixed to your local server (HTTP, gRPC, GraphQL and WebSocket), a replay runner that carries values from
one request to the next, and hundreds of built-in probes. It works with
Claude Code, Codex, Cursor, Gemini CLI and any agent that speaks
[MCP](https://modelcontextprotocol.io).

## Why agents

A fuzzer sees types and limits. It can send `age: "abc"` and notice a 500.
It cannot know that after a `DELETE` the next `GET` should be a 404, that one
user must not read another user's orders, or that a total must not come from
the request. An agent that reads the handler can know these things, so the
agent decides what to test. ReplayHarness makes the testing safe and
repeatable.

## What it looks like

The repo includes a small users API with planted bugs, [`examples/buggyapi`](examples/buggyapi).
We gave a fresh project with only its Go code, and no spec, to Claude Code
with ReplayHarness installed, and asked: *"Use the replayharness skill to find
bugs in this API. At most 6 replays."* The discovery agent mapped 7
endpoints. The replay agent wrote 6 replays, all of which failed, and
reported:

```text
1. GET /users/{id} still returns a user after DELETE returned 204; delete never removes the row (buggyapi.go:135-136).
2. POST /users without age returns 500 with a stack trace; unchecked type assertion (buggyapi.go:93).
3. GET /users/0 returns 500 with a stack trace; no id < 1 check (buggyapi.go:113-117).
4. GET /search with q under 3 characters drops the connection; q[:3] has no length check (buggyapi.go:150).
5. GET /users/{id} returns age as a string, not an integer (buggyapi.go:118-119).
6. DELETE /users/{id} succeeds with no credentials (buggyapi.go:122-123).
```

Bug 1 is out of reach for spec-based tools: every single response in it is
valid. Only the sequence of requests shows the bug. The map the discovery
agent writes looks like [`examples/buggyapi/api.yaml`](examples/buggyapi/api.yaml),
and the replays look like [`examples/buggyapi/replays.yaml`](examples/buggyapi/replays.yaml):

```text
$ replayharness replay examples/buggyapi/replays.yaml --target http://127.0.0.1:8080
✗ a deleted user is gone
    why: deleteUser answers 204 after the existence check but never removes the user (buggyapi.go:135)
    1. POST /users → 201 ok
    2. DELETE /users/5 → 204 ok
    3. GET /users/5 → 200 FAIL
       - status 200, expected 404
       body: {"age":"20","email":"temp-75cd67e3@example.com","id":5,"name":"Temp"}
       curl -sS -i 'http://127.0.0.1:8080/users/5'
✗ a two-letter search works
    why: search slices q[:3] without checking the length; minLength is 1 (buggyapi.go:150)
    1. GET /search?q=al → no response FAIL
       - error connection: no response: the server may have crashed or dropped the request
✓ search honors the limit
...
3 passed, 7 failed
```

## Set up

**1. Install the binary.** It is a single file with no dependencies:

```sh
curl -fsSL https://raw.githubusercontent.com/aryehlev/ReplayHarness/main/install.sh | sh
```

The script downloads the binary for your OS and CPU from the
[latest release](https://github.com/aryehlev/ReplayHarness/releases/latest),
checks its SHA-256, and puts it in `/usr/local/bin` or `~/.local/bin`.

You can also download it from [Releases](https://github.com/aryehlev/ReplayHarness/releases/latest)
yourself. There are builds for macOS, Linux and Windows, on amd64 and arm64.
If you download it with a browser on macOS, clear the quarantine flag once:
`xattr -d com.apple.quarantine replayharness`.

**2. Start your service locally**, with test data, for example on `http://localhost:8080`.

**3. Connect your agent.** Run these from your project's directory.

Claude Code:

```sh
claude mcp add replayharness -- replayharness mcp --target http://localhost:8080
replayharness skill --install     # the skill and the two agents, in .claude/
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.replayharness]
command = "replayharness"
args = ["mcp", "--target", "http://localhost:8080"]
```

Cursor (`.cursor/mcp.json`) and Gemini CLI (`.gemini/settings.json`):

```json
{ "mcpServers": { "replayharness": { "command": "replayharness", "args": ["mcp", "--target", "http://localhost:8080"] } } }
```

Agents without subagents run both steps themselves. Give them the playbook
with `replayharness skill >> AGENTS.md`.

If the API needs credentials, add them to the `mcp` command:
`-H "Authorization: Bearer $TEST_TOKEN"`. They go with every request except
steps marked `no_auth`, and they are redacted in the `curl` commands that
agents see.

**4. Ask:** *"Find bugs in this API with replayharness."* You can also name
an area: *"Check the orders endpoints with replayharness."*

## The two steps

### 1. Discovery: `.replayharness/api.yaml`

The [discovery agent](agents/replayharness-discovery.md) starts from what
ReplayHarness finds on its own:
* an OpenAPI or Swagger spec, if there is one
* gRPC services in `.proto` files, or from the running server by reflection
* GraphQL queries and mutations in a schema (`.graphql` files or `gql`
  literals), or from the running server by introspection
* routes found in the code by pattern, for Go, Express, Nest, Fastify,
  FastAPI, Flask, Django, Spring, axum, actix, Rails and Laravel

Then it reads the router and every handler and writes the map:

```yaml
endpoints:
  - method: POST
    path: /users
    handler: api/users.go:78
    body:
      example: {name: Ada Lovelace, email: ada@example.com, age: 36}
      required: [name, email]
      fields: {age: {type: integer, min: 0, max: 150}, email: {format: email}}
    responses: {"201": the created user, "400": invalid JSON or missing name}
    rules:
      - email is read with `email, _ :=` and never checked (users.go:91)
```

ReplayHarness validates the map and reports any route in the code that the
map leaves out. If there is also a spec, the two are merged: the map adds
handler locations, rules and auth to the spec's endpoints.

Other protocols use the same map, with one key in place of `method` and
`path`:

```yaml
  - grpc: orders.v1.Orders/CreateOrder      # the request message is the body
    handler: server/orders.go:159
    body: {required: [customer]}
    rules: [customer is REQUIRED in orders.proto but never checked (orders.go:160)]
  - graphql: mutation deleteProduct          # path defaults to /graphql
    auth: true
  - websocket: /ws/chat                      # body.example is one message
    body: {example: {type: join, room: general}}
```

### 2. Replays: `.replayharness/replays/*.yaml`

The [replay agent](agents/replayharness-replay.md) first runs the built-in
probes as a baseline. Then it writes replays for what the probes cannot know,
runs them, and checks every failure against the code before it reports one:

```yaml
replays:
  - name: a deleted user is gone              # the behavior, stated as a fact
    why: deleteUser returns 204 but never removes the row (api/users.go:88)
    steps:
      - method: POST
        path: /users
        json: {name: Ada, email: "ada-{{unique}}@example.com"}
        expect: {status: 201, json: {$.id: "<integer>"}}
        save: {id: $.id}                      # later steps use {{id}}
      - {method: DELETE, path: "/users/{{id}}", expect: {status: 204}}
      - {method: GET, path: "/users/{{id}}", expect: {status: 404}}
```

* **Steps:** `method`, `path`, `query`, `headers`, `json` or `body`,
  `no_auth`, `expect`, `save`.
* **Other protocols:**
  * gRPC: `grpc: pkg.Service/Method` with `message` (or `messages` for a
    client stream), and `expect.code: NOT_FOUND`.
  * GraphQL: `graphql: "query { ... }"` with `variables`, sent as POST to
    `/graphql` unless the step gives `method` or `path`.
  * WebSocket: `websocket: /path` with `send: [messages]` and `receive: N`.
    The replies come back as a JSON array, and `expect.close` checks the
    close code.
* **`expect`:**
  * `status`: `404`, `[200, 201]` or `"4xx"`.
  * `json`: `$.path: value`, or a matcher such as `<integer>`, `<absent>` or
    `<nonempty>`.
  * `headers`, `contains`, `not_contains`, `max_ms`.
* **`save`:** keeps a value from a response, such as an id or a `Location`
  header, for later steps.
* **Built-in failures:** a 5xx, a dropped connection, a timeout or a leaked
  stack trace fails a step even when no assertion mentions it. So do the
  same failures in other protocols: gRPC `INTERNAL` or `UNKNOWN`, a GraphQL
  error that looks like a crash, and a WebSocket closed with 1011 or dropped
  without a close frame.

The full format is in the [replay agent's instructions](agents/replayharness-replay.md#replay-format).

Replays are plain files, so you can rerun them after you change the code
(`replayharness replay .replayharness/replays --target URL`). This keeps a
fixed bug fixed. It is optional, and also works in CI.

## Tools

| Tool | What the agent gets |
|---|---|
| `discover` | Endpoints with parameters, body, responses, auth, the handler's `file:line` and the rules from the map. Reads `.replayharness/api.yaml`, any OpenAPI/Swagger spec, routes in the code, the running server's spec or recorded traffic. Reports routes the map misses and errors in the map. |
| `send_request` | One request (HTTP, gRPC, GraphQL or WebSocket) and its full response, plus problems such as a 5xx or a leaked stack trace. |
| `run_replays` | Runs replays, inline or from files. Each failing step comes back with the reason and a `curl`, `grpcurl` or `websocat` command. |
| `probe` | Built-in requests for every endpoint: missing fields, wrong types, limits, malformed bodies, missing credentials. Findings come back grouped. See [docs/probes.md](docs/probes.md). |

## gRPC, GraphQL and WebSocket

The [`examples/protocols`](examples/protocols) demo serves all three, with
planted bugs, an [API map](examples/protocols/api.yaml) and
[replays](examples/protocols/replays.yaml):

```sh
go run ./examples/protocols/cmd/protocols-demo &    # HTTP on :8081, gRPC on :9091
replayharness replay examples/protocols/replays.yaml --target http://127.0.0.1:8081 --grpc 127.0.0.1:9091
```

```text
✗ an unknown order is NOT_FOUND
    why: getOrder returns a plain error for a missing order, which gRPC sends as UNKNOWN (grpc.go:198)
    1. gRPC orders.v1.Orders/GetOrder → UNKNOWN FAIL
       - code UNKNOWN ("sql: no rows in result set (order 999999)"), expected NOT_FOUND
       grpcurl -plaintext -d '{"id":999999}' 127.0.0.1:9091 orders.v1.Orders/GetOrder
✗ the member count drops when a client leaves
    why: the chat never removes a member whose connection ended (ws.go:32)
    2. WS /ws/chat → 101 FAIL
       - $[0].members is 2, expected 1
       printf '%s\n' '{"name":"bob","room":"lobby-7371e6f9","type":"join"}' | websocat 'ws://127.0.0.1:8081/ws/chat'
...
5 passed, 12 failed
```

* **gRPC** needs no generated code. ReplayHarness reads the `.proto` files
  in the repo (`--proto` adds more) or asks the server by reflection, and
  sends messages as JSON. `--grpc host:port` sets the gRPC server when it is
  not the HTTP one, and `--target grpc://host:port` tests a gRPC-only
  server. Codes are checked like HTTP statuses: `INTERNAL`, `UNKNOWN` and
  `DATA_LOSS` are server errors, and a REQUIRED field (`field_behavior`)
  that is accepted when missing is a finding. Streams are supported: a
  server stream's messages come back as a JSON array.
* **GraphQL** probes send each query and mutation with missing, wrong-type
  and extreme variables, and check the shape of the endpoint: malformed
  bodies, unknown fields and a mutation sent with GET, which must not run.
  A response is a failure when its errors look like a crash, such as
  `internal system error` or a stack trace, even with status 200.
* **WebSocket** probes send malformed, wrong-type and oversized messages,
  and check that the server answers or closes cleanly instead of closing
  with 1011 or dropping the connection.

## Without an agent

```sh
replayharness run . --target http://localhost:8080          # built-in probes for every endpoint
replayharness run . --target http://localhost:8080 --grpc localhost:9090   # and a gRPC server
replayharness run . --target http://localhost:8080 --baseline http://localhost:9090   # compare two versions
replayharness record --upstream http://localhost:8080 -o traffic.jsonl   # record real traffic through a proxy
replayharness replay traffic.jsonl --target http://localhost:8080 --mutate   # replay it, and mutate it
replayharness context .                                     # what the agents see
```

[docs/probes.md](docs/probes.md) lists every probe and check, and describes
the record, replay and baseline modes.

## Safety

* **Only your own servers.** Requests go only to `localhost` and private
  addresses unless you pass `--allow-remote`. Pass it only for a host you
  own. Agents cannot change the target, because replay paths must start
  with `/`.
* **Requests write data.** Replays and probes create, change and delete
  records. Use a disposable environment with test data, never production.
* **Gentle by default.** At most 50 requests per second, a 10s timeout, and
  at most 250 built-in probes per endpoint.
* **Recordings leave out credentials** (`Authorization`, `Cookie`, API
  keys), unless you pass `--keep-secrets`.

## Limits

* HTTP bodies must be JSON or forms. gRPC messages are written as JSON
  (protobuf's JSON mapping). Bidirectional gRPC streams send all their
  messages, then read the replies. GraphQL subscriptions are not probed.
* Replays run one after another, so race conditions are not exercised yet.
* Agents can be wrong. The replay agent checks each failure against the
  code, and every bug report comes with a `curl` command you can run
  yourself.

## Layout

```text
cmd/replayharness     the CLI
agents/               the discovery and replay agents
skills/replayharness  the skill that runs them in order
internal/mcp          the MCP server (JSON-RPC over stdio)
internal/replay       the replay format and runner
internal/discover     API maps, OpenAPI/Swagger, .proto, GraphQL schemas, routes in code, traffic
internal/grpcx        the gRPC client: .proto compiling, reflection, dynamic messages
internal/gql          GraphQL schemas, introspection and query building
internal/wsx          the WebSocket client
internal/agent        what discover shows an agent
internal/generate     the built-in probes
internal/runner       sending requests: target check, rate limit, timeout
internal/check        the checks that turn responses into findings
internal/report       grouping, terminal output, JSON and JUnit
internal/record       the recording proxy
examples/buggyapi     the demo API with planted bugs, its map and its replays
examples/protocols    the gRPC, GraphQL and WebSocket demo, its map and its replays
```

`go test ./...` runs end-to-end tests against both demos, with and without
their specs and maps.

## License

Dual-licensed under [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at your option.
