# ReplayHarness

**A test harness for coding agents.** Your agent reads your API's handlers and
writes the requests that should break them. ReplayHarness sends those requests
to your local server, carries state from one request to the next, checks the
answers, and keeps the checks as tests that run in CI.

**Works with:** Claude Code, Codex, Cursor, Gemini CLI and any agent that speaks
[MCP](https://modelcontextprotocol.io). A plain CLI works without an agent.

## Why agents

A fuzzer only sees the spec. It can send `age: "abc"` and notice a 500. It
cannot know that after a `DELETE` the next `GET` should be a 404, that one
user must not read another user's orders, or that a page size of 0 should be
refused. Your coding agent can, because it reads the handler.

What the agent lacks is a safe, repeatable way to act on that: send requests
to the right server, reuse the id it just created, check the response, and
keep the check. ReplayHarness provides that:

* **discover:** every endpoint with its contract and the `file:line` where
  its handler is registered, so the agent knows which code to read.
* **flows:** a small YAML format for multi-step checks with saved values
  (`{{id}}`) and assertions.
* **send_request** to explore, and **probe** for a fast baseline: hundreds of
  built-in requests with missing fields, wrong types, values past limits,
  malformed bodies and missing credentials.
* **One fixed target.** Requests go only to the local server you named when
  ReplayHarness started.

## What it looks like

The repo includes a small API with planted bugs ([`examples/buggyapi`](examples/buggyapi)).
Here is a real Claude Code session that was given the ReplayHarness tools and
asked to check that a deleted user is gone:

```text
Discover found 6 endpoints.
The flow "a deleted user is gone" failed at step 3, `GET /users/8`, which returned 200 instead of 404.
Steps 1 (create, 201) and 2 (delete, 204) passed.
This is a real bug: `deleteUser` answers 204 without removing the user (buggyapi.go:135).
```

No spec-based tool can find that bug: every single response is valid. The
flows an agent writes for the demo are in [`examples/buggyapi/flows.yaml`](examples/buggyapi/flows.yaml),
and running them gives:

```text
$ replayharness check examples/buggyapi/flows.yaml --target http://127.0.0.1:8080
✓ the service reports healthy
✗ a created user can be fetched with the same fields
    why: createUser stores age as an int, but getUser sends strconv.Itoa(u.Age) (buggyapi.go:118)
    1. POST /users → 201 ok
    2. GET /users/4 → 200 FAIL
       - $.age is "36", expected 36
       curl -sS -i 'http://127.0.0.1:8080/users/4'
✗ a deleted user is gone
    why: deleteUser answers 204 after the existence check but never removes the user (buggyapi.go:135)
    1. POST /users → 201 ok
    2. DELETE /users/5 → 204 ok
    3. GET /users/5 → 200 FAIL
       - status 200, expected 404
✗ deleting a user requires a bearer token
    2. DELETE /users/6 → 204 FAIL
       - status 204, expected 401 or 403
✗ age is optional when creating a user
    1. POST /users → 500 FAIL
       - error server-error: returned 500
       - warning error-leak: response body contains a Go stack trace
✗ a two-letter search works
    1. GET /search?q=al → no response FAIL
       - error connection: no response: the server may have crashed or dropped the request
✓ search honors the limit
✓ malformed JSON is rejected
...
3 passed, 7 failed
```

## Set up

**1. Install.** It is a single binary with no dependencies, for macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/aryehlev/ReplayHarness/main/install.sh | sh
```

The script downloads the binary for your OS and CPU from the
[latest release](https://github.com/aryehlev/ReplayHarness/releases/latest),
checks its SHA-256, and puts it in `/usr/local/bin` or `~/.local/bin`.

Or download it yourself from [Releases](https://github.com/aryehlev/ReplayHarness/releases/latest).
There are builds for macOS, Linux and Windows on amd64 and arm64. If you
download it with a browser on macOS, clear the quarantine flag once:
`xattr -d com.apple.quarantine replayharness`.

With Go 1.22 or newer installed, `go install github.com/aryehlev/ReplayHarness/cmd/replayharness@latest` also works.

**2. Start your service locally** with test data, for example on `http://localhost:8080`.

**3. Connect your agent.** Run this from your project's directory.

Claude Code:

```sh
claude mcp add replayharness -- replayharness mcp --target http://localhost:8080
replayharness skill --install          # the playbook, in .claude/skills/replayharness
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

For agents without skills, add the playbook to their instructions:
`replayharness skill >> AGENTS.md`.

If the API needs credentials, add them to the `mcp` command:
`-H "Authorization: Bearer $TEST_TOKEN"`. They go with every request except
steps marked `no_auth`, and they are redacted in the `curl` commands the
agent sees.

**4. Ask.** For example: *"Check the orders API for bugs with replayharness."*
The [playbook](skills/replayharness/SKILL.md) tells the agent to map the API,
run the baseline probe, read each handler, write flows for what the code
should do, run them, double-check each failure against the code, and save the
flows worth keeping in `.replayharness/flows/`.

## Tools

| Tool | What the agent gets |
|---|---|
| `discover` | Endpoints with parameters, body schema, documented responses, auth, and the `file:line` of each handler. Reads OpenAPI/Swagger files and routes in the code (Go, Express/Nest/Fastify, FastAPI/Flask/Django, Spring, axum/actix, Rails, Laravel), the running server's spec, or recorded traffic. Reports drift between spec and code. |
| `send_request` | One request and the full response, plus problems such as a 5xx or a leaked stack trace. |
| `run_flows` | Runs flows, inline or from files. Returns each failing step with the reason and a `curl` command. |
| `probe` | Runs the built-in requests for every endpoint and returns grouped findings: crashes, 5xx errors, accepted invalid input, responses that break the spec, auth bypasses. See [docs/probes.md](docs/probes.md). |

## Flows

```yaml
flows:
  - name: a deleted user is gone              # the behavior, stated as a fact
    why: deleteUser returns 204 but never removes the row (api/users.go:88)
    steps:
      - method: POST
        path: /users
        json: {name: Ada, email: "ada-{{unique}}@example.com"}
        expect: {status: 201, json: {$.id: "<integer>", $.name: Ada}}
        save: {id: $.id}                      # later steps use {{id}}
      - {method: DELETE, path: "/users/{{id}}", expect: {status: 204}}
      - {method: GET, path: "/users/{{id}}", expect: {status: 404}}
```

* **Steps:** `method`, `path` (may include `?query`), `query`, `headers`,
  `json` or `body`, `no_auth`, `expect`, `save`.
* **`expect`:**
  * `status`: `404`, `[200, 201]` or `"4xx"`.
  * `json`: `$.path: value`, or a matcher such as `<integer>`, `<absent>` or
    `<nonempty>`. `$.items.length` gives an array's length.
  * `headers`, `contains`, `not_contains`, `max_ms`.

  Values compare with their JSON types, so `"36"` is not `36`.
* **`save`:** `$.json.path`, `header:Location` or `status`. Saved values work
  in paths, queries, headers, bodies and expectations. `{{unique}}` is
  random per flow run, so created emails don't collide.
* **Built-in failures:** a 5xx, a dropped connection, a timeout or a leaked
  stack trace fails a step even when no assertion mentions it.

## Keep the checks

Flows are plain files. Commit them and run them without an agent:

```sh
replayharness check .replayharness/flows --target http://localhost:8080 --junit flows.xml
```

The exit status is 1 when a flow fails. `--format json` prints machine-readable
results.

```yaml
# .github/workflows/api.yml
jobs:
  api:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - run: docker compose up -d --wait        # your service and its database
      - run: curl -fsSL https://raw.githubusercontent.com/aryehlev/ReplayHarness/main/install.sh | REPLAYHARNESS_INSTALL_DIR=/usr/local/bin sudo -E sh
      - run: replayharness check .replayharness/flows --target http://localhost:8080 --junit flows.xml
```

## Without an agent

The built-in probes and traffic tools also work from the CLI:

```sh
replayharness run . --target http://localhost:8080          # probe every endpoint, report what broke
replayharness run . --target http://localhost:8080 --baseline http://localhost:9090   # compare two versions
replayharness record --upstream http://localhost:8080 -o traffic.jsonl   # record real traffic through a proxy
replayharness replay traffic.jsonl --target http://localhost:8080 --mutate   # replay it, and mutate it
replayharness context .                                     # what discover shows an agent
```

[docs/probes.md](docs/probes.md) lists every probe category, every check, and
the record, replay and baseline modes.

## Safety

* **Only your own servers.** Requests go only to `localhost` and private
  addresses unless you pass `--allow-remote`. Only do that for a host you own.
  Agents cannot change the target: flow paths must start with `/`.
* **Requests write data.** Flows and probes create, change and delete records.
  Use a disposable environment with test data, never production.
* **Gentle by default:** at most 50 requests per second, a 10s timeout, and a
  cap of 250 built-in probes per endpoint.
* **Recordings leave out credentials** (`Authorization`, `Cookie`, API keys)
  unless you pass `--keep-secrets`.

## Limits

* Routes are found in code with patterns, not a parser. Routes built at
  runtime are missed, and router prefixes set in another file are not joined.
* Only REST over HTTP with JSON or form bodies. No GraphQL or gRPC.
* Flows run one after another, and steps run in order. Nothing runs
  concurrently, so race conditions are not exercised yet.

## Layout

```text
cmd/replayharness     the CLI
internal/mcp          the MCP server (JSON-RPC over stdio)
internal/flow         the flow format and runner
internal/agent        what discover shows an agent
internal/discover     OpenAPI/Swagger parsing, route extraction from code, traffic inference
internal/generate     the built-in probes
internal/runner       sending requests (target check, rate limit, timeout)
internal/check        the checks that turn responses into findings
internal/report       grouping, terminal output, JSON and JUnit
internal/record       the recording proxy
skills/replayharness  the agent playbook
examples/buggyapi     the demo API with planted bugs, its spec and its flows
```

`go test ./...` includes end-to-end tests against the demo API. They fail if
a planted bug is missed, if a correct endpoint is flagged, or if the MCP tools
stop working.

## License

Dual-licensed under [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at your option.
