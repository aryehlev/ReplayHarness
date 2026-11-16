---
name: replayharness
description: Check that the HTTP API in this project behaves the way its code and docs say, against a running local server. Read the handlers, write request flows that state the expected behavior, run them with the ReplayHarness tools, and save the ones worth keeping as regression tests. Use when the user asks to test, check, find bugs in or harden an API or its endpoints, or after changing API handlers.
---

# Checking an API with ReplayHarness

ReplayHarness sends the requests; you decide what to check. A rule-based tool
only sees the spec. You can read the handler and notice that `DELETE` never
deletes, that one user can read another user's orders, or that the page size
is never capped. Write the request that proves it.

Use the `replayharness` MCP tools when they are available (`discover`,
`send_request`, `run_flows`, `probe`). Otherwise use the CLI:
`replayharness context .`, `replayharness check <flows> --target URL` and
`replayharness run . --target URL`.

## Steps

1. **Map the API.** Call `discover` (or `replayharness context .`). Each
   endpoint comes with its parameters, body schema, documented responses and
   the `file:line` where its handler is registered. Note any spec drift.
2. **Get a baseline.** Call `probe`. It sends missing fields, wrong types,
   values at and past limits, malformed bodies, unusual path and query values,
   requests without credentials and undefined methods, and reports crashes,
   5xx errors, accepted invalid input and responses that break the spec.
3. **Read the handlers.** Open each handler at its `file:line`, follow it into
   the code it calls, and list the behavior it should have. Look hardest at
   what a generic probe cannot know:
   - **State across requests:** create then fetch, update then fetch, delete
     then fetch, delete twice, create a duplicate.
   - **Who may do what:** missing credentials, another user's resource, a role
     without permission. Only use credentials the user gave you.
   - **Validation the code or docs promise:** required fields, ranges, formats,
     enums, and what happens with empty strings and nulls.
   - **Edges:** page sizes of 0, negative values and huge values; the last page;
     sorting and filtering; very long or unicode input.
   - **Contract:** status codes, response fields and their types, error
     bodies, `Location` headers after a create.
   - **Error paths:** what the handler does when a lookup fails, a parse fails
     or a downstream call fails.
4. **Write flows.** Name each one with the behavior as a fact ("a deleted user
   is gone"). Put your reasoning and the `file:line` in `why`. Save ids with
   `save: {id: $.id}` and use them as `{{id}}`. Use `{{unique}}` in values that
   must not collide between runs (emails, usernames). Use `no_auth: true` for a
   step that must be sent without credentials.
5. **Run them** with `run_flows`. For each failure, read the code again and
   decide whether the server is wrong or your expectation is. Fix wrong
   expectations and rerun. Use `send_request` to look at a response in full.
6. **Keep the good ones.** Save flows that should keep passing to
   `.replayharness/flows/<area>.yaml`. Flows that fail because of a real bug
   can stay as a failing check until the bug is fixed. They run in CI with
   `replayharness check .replayharness/flows --target URL`.
7. **Report.** For each bug, give the flow name, the failing step, the curl
   command, the code location and a suggested fix. Change the code only if
   the user asked you to.

## Flow format

```yaml
flows:
  - name: a deleted user is gone
    why: deleteUser returns 204 but never removes the row (api/users.go:88)
    steps:
      - method: POST
        path: /users
        json: {name: Ada, email: "ada-{{unique}}@example.com"}
        expect: {status: 201, json: {$.id: "<integer>", $.name: Ada}}
        save: {id: $.id}
      - {method: DELETE, path: "/users/{{id}}", expect: {status: 204}}
      - {method: GET, path: "/users/{{id}}", expect: {status: 404}}
```

- Step fields: `method`, `path` (may include `?query`), `query`, `headers`,
  `json` (a JSON body) or `body` (a raw body), `no_auth`, `expect`, `save`.
- `expect.status`: `404`, `[200, 201]` or `"4xx"`. Without it, any status
  below 500 passes.
- `expect.json`: `$.path: value`, or a matcher: `<exists>` `<absent>` `<null>`
  `<string>` `<number>` `<integer>` `<bool>` `<array>` `<object>` `<nonempty>`.
  `$.items.length` gives an array's length. Values compare with their JSON
  types, so `"36"` does not equal `36`.
- `expect.headers` (`Location: /users/`, `<exists>`, `<absent>`),
  `expect.contains`, `expect.not_contains`, `expect.max_ms`.
- `save`: `$.json.path`, `header:Location` or `status`. Saved values work in
  paths, query values, headers, bodies and expected values (`$.id: "{{id}}"`).
  A path may be a saved path on its own (`path: "{{location}}"`).
- A 5xx, a dropped connection, a timeout or a leaked stack trace fails a step
  even when no assertion mentions it. A step that expects a 5xx is the exception.

## Rules

- Requests only go to the target ReplayHarness was started with. Do not try to
  reach other hosts.
- Requests create, change and delete data. That is fine on the local server
  you were given. Never point ReplayHarness at production.
- Do not invent credentials or try to get around authentication. Test with
  the credentials the user provided, and without any.
