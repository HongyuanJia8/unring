# Complete local demo

This directory exercises every unring capability without a GitHub, Slack, agent,
or other external account. It starts a throwaway PostgreSQL cluster and a loopback
HTTPS service with a generated one-day CA. The fake `gh` command only appends to a
local file. Everything generated lives in `pgdata/`, `pg.log`, or `runtime/`, all of
which are ignored by git and removed by `stop-db.sh`.

Prerequisites are the project's declared toolchain plus PostgreSQL's `initdb`,
`pg_ctl`, `createdb`, and `psql`, and `curl`. From the repository root, build the
working tree you want to evaluate, then enter this one directory:

```sh
go build -o unring ./cmd/unring
cd examples/demo
./start-db.sh
./start-service.sh
./check.sh
```

The first `check.sh` is the independent baseline: two accounts, two projects owned
by account 1, four events, no DDL demo table, and empty fake-service and fake-`gh`
logs. `check.sh` connects directly to the real PostgreSQL socket and reads the fake
service's append-only received-request files; none of those checks uses unring's
review or audit data.

## 1. Inserts, updates, deletes, and a cascading delete

```sh
./db-changes.sh --discard
./check.sh
```

Expect the wrapped `psql` to see its own insert and update and to see account 1 and
its two projects disappear. The review should list inserted, updated, and deleted
rows; the project deletes are caused by `ON DELETE CASCADE`. Because this run
discards, the direct check must still show the original two accounts and two
projects. Rerun with `--commit` if you also want to exercise persistence, confirm it
with `check.sh`, then run `./reset-db.sh` before continuing.

## 2. PostgreSQL DDL rolls back

```sh
./ddl-rollback.sh
./check.sh
```

The child successfully creates, inserts into, and selects from
`demo_ddl_rollback`. The review lists the schema change. The direct check must show
an empty `ddl_table` value afterward, proving the table does not exist outside the
discarded PostgreSQL transaction. This transactional DDL guarantee is why the demo
and unring's database support are PostgreSQL-only.

## 3. `TRUNCATE` has an exact count

```sh
./truncate.sh
./check.sh
```

Inside unring the count is zero. The review should say exactly four rows were
deleted from `demo_events`, rather than reporting an unknown effect. The direct
check must still show `event_count = 4` after discard.

## 4. Non-transactional `VACUUM` approval

Run this interactively and type `y` at the irreversible PostgreSQL prompt:

```sh
./vacuum.sh
```

Expect `VACUUM` to run on a separate connection, the final review to list it under
approved irreversible actions, and the `NOT FULLY REVERSIBLE` warning to remain even
though the final decision is discard. The audit exposes unring's own record:

```sh
./audit-log.sh
```

The newest JSON record must contain the exact `VACUUM demo_accounts` statement in
`postgres.irreversible_actions` with an approved decision. `./check.sh` also reads
PostgreSQL's `pg_stat_user_tables.last_vacuum` directly; it must now contain a real
timestamp even though the session was discarded. Run `./reset-db.sh`, rerun
`./vacuum.sh`, and type `n` to confirm a decline leaves `last_vacuum` empty and does
not create an irreversible action or warning.

## 5. A stageable HTTPS request is sent only on commit

First establish the negative case and inspect the real service log:

```sh
./https-stage.sh --discard
./check.sh
```

The child receives the adapter's explicit synthetic `202` response. The review says
the request was staged and discarded. `received.ndjson` must still be empty. Now:

```sh
./https-stage.sh --commit
./check.sh
```

The review says the pending call will be sent on commit. The direct log must now have
one `POST /stage` line containing the original body and a non-empty
`demo-stage:...` idempotency key. That file is the service's truth, independent of
unring's claim.

## 6. A needs-approval HTTPS call, declined and approved

Run the same command twice. Type `n` the first time and `y` the second:

```sh
./https-approval.sh
./check.sh
./https-approval.sh
./check.sh
```

On decline, curl receives HTTP 403 and exits 22; the review says the request was not
sent, and the direct service log gains no `/approval` line. On approval, curl receives
the fake service's real `201`; the review warns that discard cannot undo it, and the
direct log gains exactly one `POST /approval` line. This final discard deliberately
demonstrates that approval is not staging.

## 7. The `gh` PATH shim gates a mutation

Again run twice, declining and then approving:

```sh
./gh-mutation.sh
./check.sh
./gh-mutation.sh
./check.sh
```

The first run returns non-zero without running fake `gh`; `gh-received.log` remains
empty. After approval, the child prints a fake issue URL and the independent log gains
one exact `issue create ...` invocation. No GitHub request or credential is involved.
The script commits the session after approval so unring does not attempt GitHub issue
compensation against the intentionally fake URL.

## 8. No-database mode

```sh
./no-database.sh
```

Expect the wrapped child to print `child check: no database connection variables
were injected`, the fake service to return `ok` through HTTPS interception, and the
review to state that PostgreSQL was not configured/intercepted. The child performs
that environment check itself before curl runs, independently of the review text.
`./check.sh` then connects directly and must show the same real database state.

## 9. Audit log

```sh
./audit-log.sh
```

Expect a newest-first session list followed by `unring log --json` output. Compare its
PostgreSQL, HTTPS, approval, and `gh` records with `./check.sh`: the audit says what
unring recorded, while the direct database and received-request files say what
actually persisted. The script then bypasses `unring log`, lists the real JSON files
under `runtime/unring-state/logs/`, and prints the newest file directly. Its session ID
and fields must match the CLI output; this independently confirms that the record is
durable rather than merely held or reconstructed by the command.

## Clean shutdown

```sh
./stop-db.sh
```

This stops both local processes and removes the database cluster, socket, logs,
generated certificates, received-request files, built fake service, and demo audit
state. A second invocation is safe and should leave only the tracked demo files.
