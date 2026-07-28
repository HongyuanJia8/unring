# HTTP adapters

Adapters are declarative YAML files that tell unring how to treat intercepted
HTTPS requests. They contain no Go code and use no LLM. The GitHub and Slack
adapters shipped with unring are ordinary files in
[`internal/adapter/builtin`](../internal/adapter/builtin) and pass through the
same loader as user files.

Set `UNRING_ADAPTERS` to an OS path-list of additional files:

```sh
UNRING_ADAPTERS="$PWD/adapters/mail.yaml:$PWD/adapters/webhooks.yaml" \
  unring run -- your-agent
```

On Windows, use `;` instead of `:`. User files are evaluated before the shipped
files. The first matching rule whose `when` expression is true wins. Duplicate
adapter names, unknown YAML fields, invalid expressions, unreadable files, and
invalid schema values abort startup and name the offending file. An adapter is
never silently skipped.

## Complete example

This adapter stages one kind of notification and requires approval for other
actions on the same endpoint:

```yaml
version: 1
name: acme-notifications
rules:
  - name: async-notification
    match:
      hosts:
        - api.acme.example
        - "*.regional.acme.example"
      methods:
        - POST
      path: /v1/projects/*/actions
    when: >-
      body.kind == "notification" &&
      "x-delivery-mode" in request.headers &&
      request.headers["x-delivery-mode"] == "async"
    tier: stageable
    idempotency_key: >-
      "acme-notification:" + request.body_sha256
    response:
      status: 202
      headers:
        Content-Type: application/json
        X-Unring-Staged: "true"
      body: '{"accepted":true,"unring":{"staged":true,"real_response":false}}'
    undo:
      method: DELETE
      url: https://api.acme.example/v1/notifications/${response.id}

  - name: other-action
    match:
      hosts:
        - api.acme.example
        - "*.regional.acme.example"
      methods:
        - POST
      path: /v1/projects/*/actions
    tier: needs-approval
    idempotency_key: >-
      "acme-action:" + request.body_sha256
```

## Schema

Top-level fields:

| Field | Required | Meaning |
|---|---:|---|
| `version` | yes | Schema version. The current and only value is `1`. |
| `name` | yes | Unique adapter name used in review and audit records. |
| `rules` | yes | Non-empty ordered list of request rules. |

Rule fields:

| Field | Required | Meaning |
|---|---:|---|
| `name` | yes | Unique rule name within the adapter. |
| `match.hosts` | yes | Exact hostnames or `*.` suffix wildcards, without a scheme or port. |
| `match.methods` | yes | Uppercase HTTP methods. |
| `match.path` | yes | Go-style path glob. `*` matches within one path segment. |
| `when` | no | CEL boolean expression. A false result continues to the next rule. |
| `tier` | yes | `stageable`, `needs-approval`, or `already-irreversible`. |
| `idempotency_key` | required for `stageable` | CEL string expression. |
| `response` | required for `stageable` | The response returned without contacting the origin. |
| `undo` | no | Compensating HTTP request metadata reserved for M8. It is validated and retained but is not executed yet. |

`response.status` must be a 2xx status. `response.headers` must include
`X-Unring-Staged: "true"` and cannot contain hop-by-hop headers.
`response.body` is a static string.

Every synthesized response must say that it is staged and not a real service
response. It should include only the minimum fields the client needs to accept
the call. Do not invent server IDs, timestamps, canonical URLs, version
numbers, or any value a later call might consume. For example, the Slack
response says `ok: true` and carries unring's staged marker, but deliberately
does not claim a Slack `ts`, channel, or message object.

On commit, unring sends the original method, URL, headers, and body and sets the
`Idempotency-Key` header to the evaluated key. On discard, it deletes the
in-memory staged call without sending it. Request bodies are available in the
live review but are not serialized into audit JSON.

`undo` currently accepts:

| Field | Required | Meaning |
|---|---:|---|
| `method` | yes | Compensating HTTP method. |
| `url` | yes | Absolute compensating URL; `${...}` placeholders are retained for M8. |
| `body` | no | Compensating request template retained for M8. |

## CEL environment

CEL was selected instead of JSONPath because rules need deterministic boolean
logic across request metadata and bodies, not only value selection. Expressions
are compiled at startup. CEL has no network access and no model call.

The following values are available:

| Expression | Type | Value |
|---|---|---|
| `request.method` | string | Uppercase HTTP method. |
| `request.url` | string | Complete intercepted URL. |
| `request.host` | string | Hostname without port. |
| `request.path` | string | Escaped URL path. |
| `request.query` | map | Query values; one value is a string, repeated values are a list. |
| `request.headers` | map | Lowercase header names to comma-joined strings. |
| `request.body_sha256` | string | Lowercase hex SHA-256 of the original body. |
| `request.body_size` | integer | Original body length in bytes. |
| `body` | dynamic | Parsed JSON object/array, parsed form map, or the raw body string. |

Guard optional map fields with CEL's `in` operator before indexing them:

```cel
"mode" in body && body.mode == "asynchronous"
```

## Fallback HTTP heuristics

Adapters run first. If no rule matches, unring forwards methods defined by HTTP
as safe (`GET`, `HEAD`, `OPTIONS`, and `TRACE`) and records that they already
ran. Every other method requires explicit approval. A missing approval callback,
a declined prompt, or a classification error blocks the request; it never
quietly forwards.

Plain HTTP is still blocked and reported as un-intercepted because unring cannot
provide the same confidentiality and classification guarantees without TLS
interception. CONNECT passthrough and protocol upgrades are also listed as
un-intercepted.
