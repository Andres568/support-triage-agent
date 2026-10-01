# 0007 — Cost in integer micro-USD from a dated price table, frozen at write time

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Cost per correctly resolved ticket is a promotion and retirement criterion
(`docs/autonomy.md`), and a run can be capped by spend. Costs are tiny per
call (fractions of a cent), summed over many calls, and prices change over
time. Floating-point dollars drift when summed; recomputing history from
today's prices rewrites the past.

## Decision

- **Integer micro-USD.** `providers.Price` holds micro-USD per million
  tokens for input, output, cache read and cache write. `Price.Cost(usage)`
  is integer math: `Σ tokens × microsPerMTok / 1_000_000`, rounded down
  once, at the end.
- **Cost the aggregate, not the parts.** `Cost` is meant for aggregated
  `Usage`: `ItemRecorder.Usage` sums an item's steps and costs the total
  once, which is `run_items.cost_micros`. Each `run_steps.cost_micros` is
  the step's own cost, so step costs may sum to slightly less than the
  item's (at most one micro-USD per step).
- **A dated price table.** `providers.Models` is the single place a model is
  added or repriced; each paid entry carries `PricesVerified`
  (2026-09-29). Anthropic cache read is 0.1x input and cache write 1.25x
  (`anthropicPrice`). **OpenAI and xAI cached-input prices were not
  verified**, so `cloudPrice` charges a cache read at the full input price:
  cost is overestimated rather than hidden. Local Ollama models cost 0
  (hardware not modeled).
- **Frozen at write time.** Cost is computed when the rows are written and
  stored in `run_items` and `run_steps`. A later price change does not move
  historical cost.
- `MAX_RUN_COST_MICROS` (default 0 = no cap) stops a worker from claiming
  once the run has spent it (`cmd/worker/budget.go`).

## Consequences

- Positive: exact, summable numbers (`SELECT sum(cost_micros)`), no float
  drift; history reflects what was believed true when the work ran.
- Negative: prices are maintained by hand and can go stale (the date says
  how stale); cached OpenAI/xAI calls are over-reported until their prices
  are verified; correcting a wrong historical price needs a manual
  backfill.

## Alternatives considered

- **Float dollars:** rounding error accumulates and comparisons get fuzzy.
- **Store tokens only, price at query time:** always current prices, but
  history silently changes whenever the table does.
- **Fetch prices from provider APIs:** no stable pricing API across the
  providers used.
