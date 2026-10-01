# qwen3:4b-instruct: why support tickets hit the step limit

Re-captured 2026-10-01 after the latest measurement from a two-ticket
re-run (`make eval AGENT=support MODEL=ollama/qwen3:4b-instruct
CASES=HD-2001,HD-2003`, results not kept), read back from `run_steps` and
`run_items.transcript` on `triage_eval`. Synthetic seed; draft text and
customer names elided. Both tickets ended `agent_limit`, as in the measured
runs (19/30 on every repeat).

`run_steps` (both tickets the same shape, 8 model steps): every model step
finished `completed`, no step is an error, and the only tool calls are the
first turn's `get_order` and `search_policy`. After them the model never
calls a tool again.

HD-2001 transcript, after the tool results:

```
 3 assistant (text): { "category": "order_status", "action": "auto_reply", "draft_reply": "…", … }
 4 user:             You must finish by calling the submit tool. Do not answer in plain text.
 5 assistant (text): {"error": "Call to submit_decision must be made exactly once as the last step."}
 6 user:             You must finish by calling the submit tool. Do not answer in plain text.
 …                   (5 and 6 repeat until the step limit)
```

HD-2003: the same shape, except that every assistant turn from 3 on
repeats the decision itself as JSON text (`{"category": "order_change",
"action": "auto_reply", "confidence": 0.95, "draft_reply": "…", …}`;
the first one says draft_for_review, the rest auto_reply).

So the adapter and the tools work; the model writes its decision as plain
text instead of emitting the `submit_decision` tool call, and does not
recover when told to. Previous captures had the same outcome with a
different text (the call written out as text, in JSON or function
syntax). The wording varies; the failure does not.
