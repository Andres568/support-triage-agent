# 0008 — Postgres runs/run_items/run_steps are the source of truth; OTel GenAI spans are a view

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

We need to answer, per run, item and step: what happened, how long it
took, which model, how many tokens, what it cost, and why it failed. Traces
are good for latency trees but are sampled, exported asynchronously, kept
briefly, and leave the database's access controls. Evals and cost reports
need data that is complete and queryable next to the tickets and tasks.

## Decision

- **Rows are the record.** `queue.StartRun`/`FinishRun` write `runs` (agent,
  model, autonomy, prompt SHA, counters). `obs.ItemRecorder.Finish` writes
  one `run_items` row per attempt (outcome, source, latency, tokens, cost,
  capped error, capped transcript: ADR-0014) and its `run_steps` (one per
  model or tool call), in its own transaction.
- **Recorded by the worker, after finalize.** `worker.process` owns the
  item's span and recorder and calls `Finish` after the finalize or abandon,
  with the real outcome (final status, `abandoned`, `lost_lease`,
  `abandon_failed`). The write is best-effort with its own short timeout
  (`DefaultWriteTimeout`, 2s): a failure is logged, never fails or retries
  a decision, and never touches the leased row.
- **Spans are a view.** `run {agent}`, `process {kind}`, `chat {model}`
  (CLIENT) and `execute_tool {name}`, with GenAI attributes defined in
  `internal/obs/genai.go` (the Go semconv package dropped them; the
  convention is still in Development). `gen_ai.usage.input_tokens` reports
  input including cache. App keys: `app.cost_usd_micros`, `app.subject`,
  `app.attempt`, `app.outcome`, `app.model`.
- **No prompt or ticket text in spans or logs.** Messages, completions,
  ticket text and emails are never attributes; errors appear only as a
  low-cardinality `error.type` (`obs.errorType`, `worker.ErrorType`). Full
  error text is stored only in the database, capped.
- **Logs are Cloud Logging JSON** (`obs.NewLogger`): `severity`, `message`,
  and `logging.googleapis.com/trace`, `spanId` and `trace_sampled` from the
  span in context, so logs and traces correlate.

## Consequences

- Positive: evals and cost reports read SQL, not a trace backend; a missing
  exporter or collector loses no data (on GCP no collector is deployed yet,
  ADR-0010 amendment); spans can go to a backend with weaker access rules
  without carrying customer data.
- Negative: two write paths to keep consistent; a failed row write loses
  that item's telemetry (logged). Spans alone cannot explain a decision;
  the transcript row is needed, and it expires (ADR-0014).

## Alternatives considered

- **Traces as the only record:** sampling, retention and access rules make
  them unfit for cost accounting and audits.
- **Prompt/completion content on spans (as GenAI conventions allow):** most
  useful for debugging, but it copies customer text into the trace backend.
- **Recording inside the handler:** a panic or a lost lease would leave no
  row, and the recorded outcome would not be the real one.
