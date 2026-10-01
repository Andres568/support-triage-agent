# 0009 — Evals: what we score, and what the CI gate can and cannot catch

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Both agents make decisions that a deterministic gate then checks and caps
(support: `triage.Gate` and the autonomy policy; orders: `orders.Gate`).
Their autonomy (`docs/autonomy.md`) should change only on evidence, and a
change to the prompt, a tool, the gate or the scoring should not silently
change outcomes. The labels are small: 30 support tickets
(`evals/golden.jsonl`) and 9 orders tasks (`evals/golden_tasks.jsonl`), all on
a synthetic seed. CI has no model and no API keys.

## Decision

1. **Score before and after the gate.** "Proposed" is the model's own output;
   "final" is what the system does after the gate. Promotion is judged on
   proposed metrics (would the model be safe without the gate?), safety
   invariants on final ones (is the system safe?). The gate's work is
   counted (`gate_prevented_false_auto_replies`), so a model that leans on
   it shows up.
2. **Policy-capped expectations.** The expected final action of a ticket is
   its golden action capped by the policy (`Policy.Cap`): a golden
   auto_reply in a category that may not auto-reply can only end as a draft.
   Evals run at `-autonomy auto` by default; in shadow a false auto-reply
   is impossible and the metric would say nothing.
3. **Pre-checked tickets** (a deterministic regex escalates them before any
   model runs) count for the final metrics only; they are not in the
   denominators of category accuracy, proposed action accuracy or the
   proposed false auto-reply rate. The proposed false auto-reply and
   unsafe-eligible rates count only items with a proposal; items without
   one (agent_limit, failed) are reported as `no_proposal` next to them.
4. **Orders against a rules baseline.** Every task is also decided by
   `orders.Baseline` (hand-written rules); the LLM is kept only if its final
   verdict accuracy beats the baseline by the margin in `autonomy.md`. A
   failed task has no baseline and is counted (`baseline_unscored`), not
   scored wrong.
5. **Small n, so intervals.** Every rate carries a Wilson 95% interval, and
   runs are repeated (`REPEAT=3`); a criterion holds only if it holds on
   every repeat. One ticket is 3.3 points of support accuracy and one task
   11 points of orders accuracy: differences inside the intervals are not
   differences.
6. **Two CI layers, no model** (`make db-check`):
   - **Oracle** (`fake/oracle`, `TestOracle_EndToEnd`): each item is
     answered with its golden label through the whole pipeline (pre-check,
     loop, tools over HTTP, gate, outbox, scoring). Every rate must be at its
     perfect value, `correct == items`, the rules baseline at its known
     score, and the pre-checked tickets must be exactly a listed set (the
     oracle would escalate them anyway, so this is what catches a weakened
     pre-check). It tests the plumbing, never the model.
   - **Replay** of each committed `evals/cassettes/<agent>/ollama-qwen3-8b.json`:
     the model's recorded answers are fed back through today's code, and
     the summary (`.summary.json`, latency ignored) and each item's outcome
     (`.items.json`: source, proposed and final action and category,
     follow-ups, verdicts, baseline) must be reproduced, and the thresholds
     met. The reference lives next to the cassette: a recording is one run
     for the gate, not the measurement, which is the repeated run under
     `evals/results/`.
7. **Replays pin time.** The seed is relative to `now()`, and business days,
   the reprint window and the refund window are computed from the clock. A
   recording stores its clock and the seeded timestamps in the cassette
   (`fixture`); a replay restores both and pins the commerce store's and the
   orders handler's clock to it, so a replay on another weekday or month
   reproduces the recording exactly (`TestReplay_AnotherDay`). The restore
   fails closed unless every table has exactly the recorded rows (a seed
   change means re-record), and so does a cassette without a fixture. Request
   fingerprints leave tool-result content out for the same reason.
8. **Hard invariants are never relaxed** (`evals/thresholds.json` `max`,
   and injection pass `min` 1.0). Floors on accuracy are regression floors
   set from the measured runs, not aspirations.

## What CI does not measure

- **The model.** A replay re-runs code around frozen answers. A prompt or
  tool change makes the cassette stale (`ErrStaleCassette`, by SHA) until it
  is re-recorded locally with Ollama (`make eval-record`).
- **Paid providers.** The Anthropic and OpenAI adapters are verified against
  recorded fixtures only; no eval has run on them (no keys).
- **Latency and cost.** Replay latency only measures reading a file.
- **Timing-dependent failures.** A tool timeout during recording is not
  reproducible: tool calls run for real under replay. Model outages are
  recorded and replayed as outages.
- **Generalization.** The prompts were tuned while looking at these same 30
  tickets and 9 tasks; there is no held-out set. The measured numbers are
  in-sample and optimistic.

## Consequences

- `make check` fails if gating, parsing, scoring or the pre-check changes
  any recorded outcome, with the differing items named.
- Every prompt or tool change needs a local re-record (minutes with Ollama)
  and a reviewed diff of `.summary.json` / `.items.json`.
- Cassettes quote synthetic customer text and are committed; they are
  recorded only on the `triage_eval` seed, never on real tickets.

## Alternatives considered

- **Request-hash cassettes** (VCR style): would never match on another day,
  because tool results contain day-dependent business days.
- **Live model in CI:** needs Ollama or keys in CI, is slow and
  non-deterministic; the replay gives a deterministic regression signal and
  the measured runs are a separate, local step.
- **Pinning time in the seed** (fixed dates): the dev seed's "late" and
  "within 30 days" must stay true over time, so the seed stays relative and
  the eval harness snapshots and restores it instead.
