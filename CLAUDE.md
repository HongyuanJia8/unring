# Working in this repo

`unring` intercepts an AI agent's real-world side effects so a session can be
committed or discarded. Read [docs/PROJECT-BRIEF.md](docs/PROJECT-BRIEF.md) for the
design rationale and the feasibility results it rests on; [ROADMAP.md](ROADMAP.md)
is the task checklist and the only place work items are tracked.

## Non-negotiables

These come from the brief and are not open for quiet re-litigation in a PR:

1. **Never silently fail to intercept.** If traffic gets past us, it must be surfaced
   to the user as un-intercepted. A user who believes they are covered when they are
   not is worse off than one who was never protected.
2. **No LLM anywhere in the classification path.** Adapters, then heuristics, then
   stop and ask. Unknown means *ask*, never *guess*.
3. **Built-in adapters use the same YAML format as community adapters.** No Go-coded
   privileged path for GitHub/Slack.
4. **No partial commit.** One decision for the whole session. Partial commits create
   states nobody can reason about (`notified_at` set, mail never sent).
5. **Default to rollback.** Any crash, panic, or lost connection ends the session as
   a discard, never a commit.

## Language rules for user-facing text and docs

Never describe this as a "dry run" or a "preview". Database statements really
execute; the agent sees real results. The promise is **reversibility**, not
simulation. Do not oversell undo either — most of the value is that stageable
effects never happen at all, and compensation is the documented-limits fallback.

## Conventions

- Go, standard layout: `cmd/unring` for the entrypoint, `internal/...` for everything
  not meant as a public API.
- Postgres wire protocol work uses `github.com/jackc/pgx/v5/pgproto3`.
- Errors wrap with `%w` and carry context; no `panic` in request paths.
- Tests that need a database spin up a throwaway Postgres and skip cleanly when one
  is unavailable — `go test ./...` must pass on a machine with no Postgres.

## Quality gates

`gofmt -l .` (must be empty) · `go vet ./...` · `go build ./...` · `go test ./...`
