# unring

> **Make everything your agent does undoable.**

`unring` wraps an AI coding agent and intercepts the side effects it has on the real
world — database writes and outbound API calls. When the run is over you see exactly
what it did and what it is about to do, then you decide: **commit** or **discard**.

The name comes from *you can't unring a bell*. That is the whole point: now you can.

> **Status: Postgres transactions plus HTTPS interception and audit.** Database
> activity is transactional. HTTPS requests are intercepted, recorded, and forwarded
> immediately in this slice; classification, staging, and undo are not implemented
> yet. See [ROADMAP.md](ROADMAP.md) for what is built and what is next.

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

PostgreSQL 14 is the minimum supported version. Older servers are rejected at startup
with an explicit version error before any client traffic is accepted; CI exercises
the integration suite against PostgreSQL 14 and 17 explicitly.

`unring` opens one real transaction on the configured database and binds both the
Postgres and HTTPS proxies to ephemeral loopback ports. It injects the connection
variables into the child process only. Every Postgres client connection opened by that
child uses the same backend transaction; individual protocol exchanges are serialized
because PostgreSQL has only one backend connection. Closing one client connection
does not close the transaction.

For HTTPS, the child receives `HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`,
and `CURL_CA_BUNDLE`. Node.js, curl, and Python's standard library therefore trust
unring's local CA without changing trust for the user's shell or any other process.
Every successfully intercepted HTTPS request is forwarded in this slice and is shown
under **HTTPS REQUESTS — INTERCEPTED AND ALREADY FORWARDED** in the review. A final
discard cannot undo such a request; the warning is deliberate.

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

## Audit log

Every run creates a structured JSON audit record before the child starts and updates
it atomically as the session progresses. It includes start and end times, the requested
decision and confirmed outcome, per-table row changes, schema changes, irreversible
actions approved or declined, intercepted HTTPS requests, and anything unring saw but
could not intercept. Signal termination, a recoverable unring panic, and backend loss
all retain a record; an unknown database outcome is recorded as `unknown`, never as a
successful discard.

```sh
unring log                    # list past sessions, newest first
unring log <session-id>       # human-readable detail; an unambiguous prefix works
unring log --json <session-id>
```

The per-user state root is `$XDG_STATE_HOME/unring` when `XDG_STATE_HOME` is set,
otherwise the platform user-config directory plus `unring` (on macOS,
`~/Library/Application Support/unring`). `UNRING_STATE_DIR` is an explicit override
for isolated installations and tests.

The CA certificate is stored at `<state-root>/ca/ca.pem`; its private key is
`<state-root>/ca/ca-key.pem`, inside a mode-`0700` directory with mode-`0600`
permissions. The CA is generated once and reused. The private key is never injected or
logged—only the certificate path is passed to the child. Unring never installs the CA
in the system trust store or macOS keychain and never modifies a shell profile.
Keeping it in private per-user state gives wrapped children a stable CA across runs
without broadening trust for any process unring did not start.

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

- HTTPS classification, staging, and compensation are not implemented yet. Every
  HTTPS request that unring successfully intercepts is forwarded immediately, so an
  external effect may already have happened when the review appears.
- Go binaries on macOS, including `gh`, do not honor `SSL_CERT_FILE`; Go's
  `crypto/x509` uses the system keychain. Unring deliberately does not install its CA
  there. Such a client normally fails the MITM TLS handshake, and unring reports the
  host in the un-intercepted section and audit log. This is not silent coverage.
  The planned `gh` PATH shim is M7.
- A host can be deliberately passed through with
  `UNRING_HTTPS_PASSTHROUGH=host1,host2`. The CONNECT tunnel still uses the loopback
  proxy, but unring cannot see its requests or bodies; the host is therefore shown
  prominently as un-intercepted in both review and audit.
- Only HTTPS proxy traffic is covered. A child that overrides proxy settings, uses a
  tool-specific bypass, or opens a direct connection can evade interception. Unring
  clears inherited `NO_PROXY` for the child to avoid an invisible exclusion list, but
  it is an accident guard, not a hostile-process sandbox.
- Sequences do not roll back — discarded runs still leave gaps in auto-increment IDs.
- PostgreSQL does not expose authoritative per-table row counts for `TRUNCATE`.
  unring reports that summary as `UNKNOWN` and forces the session to discard, so a
  session containing a successful `TRUNCATE` cannot currently be committed.
- Postgres only. MySQL commits DDL implicitly, which breaks the core guarantee.
- Both PostgreSQL's simple and extended query protocols are supported. Prepared
  statement and portal names are isolated per client on the shared backend.
- Statements that must run outside a transaction (`CREATE DATABASE`, `VACUUM`,
  `CREATE INDEX CONCURRENTLY`, `ALTER SYSTEM`, `CHECKPOINT`, and similar) require
  approval and make the session not fully reversible. They are refused if the shared
  transaction already has uncommitted database changes.
- Lock-waiting maintenance commands cannot run against a table while the shared
  transaction holds locks on it. This includes concurrent index builds, `VACUUM FULL`,
  `CLUSTER`, and `REINDEX`, including their database-wide or schema-wide forms. Unring
  checks both concrete and broad targets before execution and reserves the shared
  backend through the escape operation; commit or discard the session first, then run
  the maintenance command separately. A short lock timeout remains as a backstop for
  conflicts outside the session that cannot be predicted.
- Client transaction control is mapped to private savepoints. One client-visible
  transaction may be open at a time; it does not pin the backend while idle.
- The shared transaction uses PostgreSQL's default `READ COMMITTED` isolation. Its
  catalog baseline is captured explicitly; this also lets review see approved DDL
  committed on the non-transactional connection.
- While that transaction is open, other clients may run read-only queries. Unring
  rolls those query cycles back internally so they cannot become part of the open
  client's savepoint. A concurrent write or second `BEGIN` fails immediately with
  SQLSTATE `55P03`: waiting would recreate the cross-connection deadlock this policy
  is intended to prevent.
- Connection options passed directly as child command arguments can bypass injected
  environment variables. This tool guards against accidents, not deliberate bypass.
- Some effects genuinely cannot be undone: mail that has been delivered, a message
  someone already read. The value is that most side effects never happen at all;
  compensation is only the fallback.

## License

MIT
