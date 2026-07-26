# unring

> **Make everything your agent does undoable.**

`unring` wraps an AI coding agent and intercepts the side effects it has on the real
world — database writes and outbound API calls. When the run is over you see exactly
what it did and what it is about to do, then you decide: **commit** or **discard**.

The name comes from *you can't unring a bell*. That is the whole point: now you can.

> **Status: early development.** Nothing here is usable yet. See [ROADMAP.md](ROADMAP.md)
> for what is built and what is next.

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
- Some effects genuinely cannot be undone: mail that has been delivered, a message
  someone already read. The value is that most side effects never happen at all;
  compensation is only the fallback.

## License

MIT
