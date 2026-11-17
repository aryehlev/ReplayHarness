---
name: replayharness
description: Find bugs in the API in this project (REST, gRPC, GraphQL or WebSocket) by running real requests against a local server, with or without an OpenAPI spec, .proto file or GraphQL schema. A discovery agent maps the API from the code, then a replay agent writes and runs multi-step requests that check what each handler should do. Use when the user asks to test, check, find bugs in or harden an API or its endpoints, or after changing API handlers.
---

# Testing an API with ReplayHarness

ReplayHarness works in two steps, each done by its own agent:

1. **Discovery.** The `replayharness-discovery` agent reads the code and
   writes `.replayharness/api.yaml`: every route, gRPC method, GraphQL field
   and WebSocket endpoint with its handler `file:line`,
   parameters, an example body, required fields, auth, and the rules the
   handler enforces. It needs no OpenAPI spec.
2. **Replays.** The `replayharness-replay` agent reads the map and the
   handlers, writes replays (multi-step requests with expectations) to
   `.replayharness/replays/`, runs them against the local server, and
   reports the bugs it confirms.

## What to do

1. Check that the `replayharness` MCP tools are available (`discover`,
   `send_request`, `run_replays`, `probe`) and that the server they point at
   is running: call `send_request` with a GET to a route you know. If the
   tools are missing, use the CLI instead: `replayharness context .`,
   `replayharness replay <dir> --target URL`, `replayharness run . --target URL`.
   Ask the user for the server's URL if you don't know it. A gRPC server on
   its own port is given with `--grpc host:port`.
2. If `.replayharness/api.yaml` is missing, or the routes changed since it
   was written, launch the **replayharness-discovery** agent. Wait for its
   report.
3. Launch the **replayharness-replay** agent. Give it the discovery report
   and anything the user asked to focus on.
4. Tell the user what the replay agent confirmed: each bug with its failing
   step, `curl`, `grpcurl` or `websocat` command, code location and
   suggested fix, and where the
   replays were saved.

If you cannot launch agents, do both steps yourself, in order, following
the discovery agent's instructions and then the replay agent's
(`.claude/agents/replayharness-discovery.md` and
`.claude/agents/replayharness-replay.md`; `replayharness skill` prints all
of it as one document).
