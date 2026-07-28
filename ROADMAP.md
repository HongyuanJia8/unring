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
- [x] M1.3 Serialize individual backend exchanges across concurrent clients. An idle
      client-visible transaction does not retain the backend lease: other clients can
      run read-only queries, while concurrent writes and a second `BEGIN` fail with
      SQLSTATE `55P03` rather than waiting indefinitely for an application-level
      dependency cycle
- [x] M1.4 Session decision: `COMMIT` / `ROLLBACK`, plus crash-safe default to rollback
- [x] M1.5 Extended query protocol (`Parse`/`Bind`/`Describe`/`Execute`/`Sync`) —
      required by every ORM; rewrite prepared-statement names to avoid collisions
      between clients sharing one backend (see pgbouncer's known pitfalls)
- [x] M1.6 Escape hatch for statements that cannot run in a transaction block
      (`CREATE DATABASE`, `DROP DATABASE`, `VACUUM`, `CREATE INDEX CONCURRENTLY`,
      `ALTER SYSTEM`, `CHECKPOINT`) — classify as *needs approval*, run on a separate
      non-transactional connection, mark the session as no longer fully reversible.
      Lock-waiting maintenance against relations locked by the shared transaction is
      refused up front, including broad `VACUUM FULL`, `CLUSTER`, and `REINDEX`
      operations; commit or discard first, then run it outside unring
- [x] M1.7 Change summary — what the transaction actually did, for the review screen
- [x] M1.8 Map client `BEGIN`/`COMMIT`/`ROLLBACK` and named savepoints onto private
      savepoints inside the shared transaction. This preserves the single-decision
      guarantee while letting transaction-managing clients and ORMs run
- [ ] M1.9 Count `TRUNCATE` effects authoritatively so sessions containing it can be
      reviewed and committed instead of being forced to discard

## M2 — Wrapper CLI

- [x] M2.1 `unring run -- <cmd>` — start proxies, spawn child, forward signals,
      propagate exit code
- [x] M2.2 Environment injection for the child only (`PGHOST`/`PGPORT`/`DATABASE_URL`,
      upper/lower-case HTTP and HTTPS proxy variables, `ALL_PROXY`, `FTP_PROXY`,
      `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`)
- [x] M2.3 `unring claude` and friends — thin aliases over `run`
- [x] M2.4 Read-only sessions exit silently: no prompt when nothing was written

## M3 — Review interface

- [x] M3.1 Non-interactive text summary + commit/discard prompt (unblocks end-to-end)
- [x] M3.2 Bubble Tea TUI: overall commit/discard, expandable per-item detail
      (diffs, affected rows, request bodies). **No partial commit** — by design
- [x] M3.3 Un-intercepted traffic gets its own, unmissable section

## M4 — Audit log

- [x] M4.1 Structured per-session log of what really happened
- [x] M4.2 `unring log` to inspect past sessions

## M5 — HTTPS proxy

- [x] M5.1 Local CA generation, stored per-user, injected into the child process only
- [x] M5.2 MITM proxy: interception, recording, forwarding, classification, and
      staging are implemented; proxy-aware plain HTTP fails closed and protocol
      upgrades are explicitly tunneled and reported
- [x] M5.3 CONNECT passthrough for hosts we cannot MITM, reported as un-intercepted

## M6 — Adapters

- [x] M6.1 YAML adapter schema + loader (match / tier / idempotency key / undo)
- [x] M6.2 CEL expression evaluation for conditional rules and idempotency keys
- [x] M6.3 GitHub adapter, written in the community format
- [x] M6.4 Slack adapter, written in the community format
- [x] M6.5 HTTP heuristics for unknown services, defaulting mutations to
      *needs approval*

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
- Whether approved lock-conflicting maintenance should be deferred until the final
  decision, so discard omits it entirely; this better fits the promise but changes
  what the agent observes during the session
