# 0003 — Agent failures escalate; infrastructure failures retry

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

An item can fail in two different ways. The model can fail to decide: it
runs out of steps or tokens, its output is truncated, it refuses, or the
input does not fit its context window. Or the system can fail: a provider
outage, a rate limit, a timeout, the commerce API erroring mid-loop.
Retrying the first kind usually buys the same failure at the same cost;
giving up on the second kind turns a transient outage into a lost ticket.

## Decision

`agent.IsLimit` (`internal/agent/loop.go`) is the single classifier. It is
true for `ErrMaxSteps`, `ErrBudgetExceeded`, `ErrTruncated`, `ErrRefused`
and `ErrContextWindow`.

| Condition | Result |
|---|---|
| `IsLimit(err)` | A product outcome, finalized: support records `Source: agent_limit`, `escalate`, no follow-ups; orders records `needs_human` with `agent_limit` (and the rules baseline when the order is visible). |
| Anything else from the loop (transport error, HTTP 5xx/429 after the adapter's retries, deadline) | The handler returns the error; `worker.Run` calls `Queue.Abandon`; the row is retried after its lease, and `failed` after `MAX_ATTEMPTS` (default 3). |
| Commerce infrastructure error during the loop (`tools.Watched.Failure`, i.e. not `ErrNotFound`/`ErrRejected`) | Retry, even if the model decided: a decision built on a broken tool view is not trusted. |
| Commerce error while fetching facts after the loop | Retry. |

Where the errors come from:

- Adapters map provider signals onto the neutral errors: Anthropic
  `max_tokens`/`refusal`/`model_context_window_exceeded`; Responses
  `incomplete` (`max_output_tokens`, `content_filter`), a `refusal` part,
  error code `context_length_exceeded`, and the Ollama silent-truncation
  guard (input ≥ 90% of `ContextWindow`).
- `httpjson.Post` retries 429/500/502/503/529 at most twice, honoring
  `retry-after` up to 20s; transport errors are not retried there, the queue
  retries the item.
- **Pre-call estimate.** `agent.Run` estimates the first request's input
  (`EstimateInputTokens`, bytes/3, deliberately over-counting) and returns
  `ErrBudgetExceeded` before any billed call when it exceeds `MAX_TOKENS`.
  An oversized ticket escalates for free instead of being retried.
- A handler panic becomes an error (retry), not a crashed batch.

## Consequences

- Positive: no money spent retrying a model that cannot decide; every
  limit ends with a human owning the item; outages heal by themselves up
  to `MAX_ATTEMPTS`.
- Negative: a limit hit caused by a transient model quirk is not retried,
  so the escalation rate includes some items a second try would have
  solved. A persistent outage burns `MAX_ATTEMPTS` attempts per item before
  it shows up as `failed`.
- Evals report `agent_limit` and `failed` counts separately (ADR-0009).

## Alternatives considered

- **Retry everything:** simplest, but multiplies cost on inputs the model
  cannot handle and delays the human who must handle them anyway.
- **Escalate everything:** an outage would escalate the whole queue to
  humans for a problem that fixes itself in minutes.
