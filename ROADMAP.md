# Roadmap

Working checklist for the first release (target: 10–12 weeks). Derived from
[docs/PROJECT-BRIEF.md](docs/PROJECT-BRIEF.md) §5. Each milestone is meant to be a
self-contained, reviewable change that leaves the tree green.

Legend: `[ ]` todo · `[~]` in progress · `[x]` done

---

## M0 — Scaffold

- [x] Repo, MIT license, README, roadmap
- [x] Go module, `cmd/unring` entrypoint, `make`/task targets for fmt/vet/lint/test
- [x] CI (GitHub Actions): build + vet + test on macOS and Linux

## M1 — Postgres shared-transaction proxy

The zero-compromise demo. Validated in the brief as V1/V2; build it first.

- [x] M1.1 Wire protocol skeleton — listen, answer the client handshake ourselves
      (`AuthenticationOk` / `ParameterStatus` / `BackendKeyData` /
      `ReadyForQuery{TxStatus:'T'}`), relay the simple query protocol upstream
- [x] M1.2 One shared backend transaction across every client connection in a session;
      a client `Terminate` must **not** end the transaction
- [x] M1.3 Serialize backend access across concurrent clients (documented trade-off)
- [x] M1.4 Session decision: `COMMIT` / `ROLLBACK`, plus crash-safe default to rollback
- [ ] M1.5 Extended query protocol (`Parse`/`Bind`/`Describe`/`Execute`/`Sync`) —
      required by every ORM; rewrite prepared-statement names to avoid collisions
      between clients sharing one backend (see pgbouncer's known pitfalls)
- [ ] M1.6 Escape hatch for statements that cannot run in a transaction block
      (`CREATE DATABASE`, `DROP DATABASE`, `VACUUM`, `CREATE INDEX CONCURRENTLY`,
      `ALTER SYSTEM`) — classify as *needs approval*, run on a separate
      non-transactional connection, mark the session as no longer fully reversible
- [ ] M1.7 Change summary — what the transaction actually did, for the review screen
- [ ] M1.8 Map client transaction control onto savepoints. Today `BEGIN`/`COMMIT`/
      `ROLLBACK` from a client are rejected outright, which is safe but breaks the
      many tools and ORMs that manage their own transactions. Nesting them as
      savepoints inside our shared transaction keeps the single-decision guarantee
      while letting real clients work. Belongs with M1.5 — both are prerequisites
      for "a real ORM can run under unring unmodified"

## M2 — Wrapper CLI

- [x] M2.1 `unring run -- <cmd>` — start proxies, spawn child, forward signals,
      propagate exit code
- [x] M2.2 Environment injection for the child only (`PGHOST`/`PGPORT`/`DATABASE_URL`;
      later `HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`)
- [ ] M2.3 `unring claude` and friends — thin aliases over `run`
- [ ] M2.4 Read-only sessions exit silently: no prompt when nothing was written

## M3 — Review interface

- [x] M3.1 Non-interactive text summary + commit/discard prompt (unblocks end-to-end)
- [ ] M3.2 Bubble Tea TUI: overall commit/discard, expandable per-item detail
      (diffs, affected rows, request bodies). **No partial commit** — by design
- [ ] M3.3 Un-intercepted traffic gets its own, unmissable section

## M4 — Audit log

- [ ] M4.1 Structured per-session log of what really happened
- [ ] M4.2 `unring log` to inspect past sessions

## M5 — HTTPS proxy

- [ ] M5.1 Local CA generation, stored per-user, injected into the child process only
- [ ] M5.2 MITM proxy: intercept, classify, stage or forward
- [ ] M5.3 CONNECT passthrough for hosts we cannot MITM, reported as un-intercepted

## M6 — Adapters

- [ ] M6.1 YAML adapter schema + loader (match / tier / idempotency key / undo)
- [ ] M6.2 Expression evaluation for conditional rules (CEL vs JSONPath — decide here)
- [ ] M6.3 GitHub adapter, written in the community format
- [ ] M6.4 Slack adapter, written in the community format
- [ ] M6.5 HTTP heuristics for unknown services, defaulting to *needs approval*

## M7 — `gh` PATH shim

- [ ] M7.1 Shim injected at the front of the child's `PATH` — captures structured
      intent, no certificate trust required (works around Go binaries ignoring
      `SSL_CERT_FILE` on macOS)
- [ ] M7.2 Replay approved `gh` invocations on commit

## M8 — Compensating undo

- [ ] M8.1 Undo actions declared per adapter, executed on discard
- [ ] M8.2 Slack `chat.delete`; document precisely what GitHub cannot undo

---

## Explicitly out of scope for v1

Filesystem copy-on-write (git already solves it) · MySQL · teams, approval flows,
multi-user · multi-agent concurrency control · web UI.

## Open questions

- Whether `discard` should hand feedback back to the agent for a retry
  (not in v1, but do not architect it out)
