# unring

> **Make everything your agent does undoable.**

`unring` wraps an AI coding agent and intercepts the side effects it has on the real
world — database writes and outbound API calls. When the run is over you see exactly
what it did and what it is about to do, then you decide: **commit** or **discard**.

The name comes from *you can't unring a bell*. That is the whole point: now you can.

> **Status: first Postgres vertical slice.** The shared-transaction `run` command
> works with PostgreSQL's simple query protocol. Extended-query clients and other
> side-effect types are not supported yet. See [ROADMAP.md](ROADMAP.md) for what is
> built and what is next.

## Try the Postgres slice

Set your normal PostgreSQL connection environment, then wrap a command:

```sh
export DATABASE_URL='postgresql://user:password@real-host/database'
go build -o unring ./cmd/unring
./unring run -- psql
```

Building requires cgo and a working C compiler because unring uses
`pg_query_go`/libpg_query—the PostgreSQL parser itself—to classify transaction
statements. The standard Go toolchain plus Clang on macOS or GCC/Clang on Linux is
sufficient.

`unring` opens one real transaction on the configured database, binds a proxy to an
ephemeral loopback port, and injects `DATABASE_URL` plus the standard `PG*` connection
variables into the child process only. Every client connection opened by that child
is serialized onto the same backend transaction. Closing one client connection does
not close the transaction.

After the child exits, `unring` prints the simple-query batches and asks whether to
commit or discard. Automation must choose explicitly:

```sh
unring run --commit -- your-command
unring run --discard -- your-command
```

Without a terminal or a decision flag, the session defaults to discard. SIGINT,
SIGTERM, an unring panic, or loss of the real database connection also defaults to
rollback. The child's exit code is returned after a successful decision.
If an interactive child is stopped with Ctrl-Z, unring reclaims the terminal,
terminates the stopped child, and discards the session; nested job suspension is not
preserved because an arbitrary parent process cannot be trusted to resume it safely.

Database integration tests start and stop their own throwaway PostgreSQL cluster.
They skip when PostgreSQL is not installed; CI and explicit verification require a
real server and fail instead of skipping:

```sh
make test-integration
```

## How it works

Three layers, one promise:

| Layer | Mechanism | Reversibility |
|---|---|---|
| Database | A real transaction. `discard` is a `ROLLBACK`. | Fully reversible |
| Stageable external calls | Never sent, only recorded. | Fully reversible — it never happened |
| Calls that must really run | Approved up front, compensated afterwards | Best effort, with stated limits |

Your agent is not running against a simulation. Database statements really execute,
inside a transaction the agent shares across every connection it opens — so it reads
back its own writes, gets real results, and real errors. What it does not get is the
ability to make any of it permanent without you saying so.

## Design commitments

- **Silently failing to intercept something is this project's worst failure mode.**
  Traffic we cannot intercept is reported as un-intercepted, loudly, in the review
  screen. We would rather tell you "this part got past us" than let you believe you
  were covered.
- **No LLM in the classification path.** Known services are matched by declarative
  adapters, unknown ones fall back to HTTP heuristics, and anything uncertain stops
  and asks you. A classifier that is usually right is not good enough here.
- **Built-in adapters use the exact same format as community ones.** No privileged
  path for the adapters we ship — that is how plugin formats rot.
- **This guards against accidents, not against a hostile agent.** An agent that
  deliberately routes around the proxy can.

## Honest limits

- Sequences do not roll back — discarded runs still leave gaps in auto-increment IDs.
- Postgres only. MySQL commits DDL implicitly, which breaks the core guarantee.
- This slice supports PostgreSQL's simple query protocol only. Clients using
  `Parse`/`Bind`/`Execute` fail loudly; extended-query support is M1.5.
- Statements PostgreSQL forbids inside a transaction (`CREATE DATABASE`, `VACUUM`,
  `CREATE INDEX CONCURRENTLY`, `ALTER SYSTEM`, and similar) return their real backend
  errors. The session recovers and remains usable, but the M1.6 approval escape hatch
  is not part of this slice.
- Client transaction-control statements are rejected because only unring may commit
  or roll back the shared transaction.
- Concurrent client queries are intentionally serialized on the one backend
  connection.
- Connection options passed directly as child command arguments can bypass injected
  environment variables. This tool guards against accidents, not deliberate bypass.
- Some effects genuinely cannot be undone: mail that has been delivered, a message
  someone already read. The value is that most side effects never happen at all;
  compensation is only the fallback.

## License

MIT
