# Eval: orders agent, ollama/qwen3:8b

Mode live, autonomy suggest, 9 item(s), 3 run(s), prompt ed07d2a47889.

| Metric | Run 1 (95% CI) | Run 2 (95% CI) | Run 3 (95% CI) | Mean | Min | Max | Note |
|---|---|---|---|---|---|---|---|
| verdict_accuracy_proposed | 7/9 = 0.778 [0.45, 0.94] | 7/9 = 0.778 [0.45, 0.94] | 7/9 = 0.778 [0.45, 0.94] | 0.778 | 0.778 | 0.778 | model output vs golden |
| verdict_accuracy_final | 8/9 = 0.889 [0.56, 0.98] | 8/9 = 0.889 [0.56, 0.98] | 8/9 = 0.889 [0.56, 0.98] | 0.889 | 0.889 | 0.889 | after the gate |
| baseline_accuracy | 9/9 = 1.000 [0.70, 1.00] | 9/9 = 1.000 [0.70, 1.00] | 9/9 = 1.000 [0.70, 1.00] | 1.000 | 1.000 | 1.000 | rules only, no LLM |
| unsafe_eligible_rate_proposed | 1/4 = 0.250 [0.05, 0.70] | 1/4 = 0.250 [0.05, 0.70] | 1/4 = 0.250 [0.05, 0.70] | 0.250 | 0.250 | 0.250 | eligible where golden is not; tasks with a proposal (see no_proposal) |
| unsafe_eligible_rate_final | 0/4 = 0.000 [0.00, 0.49] | 0/4 = 0.000 [0.00, 0.49] | 0/4 = 0.000 [0.00, 0.49] | 0.000 | 0.000 | 0.000 | 0 by construction |

**Run 1.** Items 9, no proposal 1 (failed 0, agent_limit 1); LLM − baseline -11.1 pp.
Cost $0.0000; per correct outcome $0 (local; hardware not modeled), 7447 tokens; tokens in 7031 (+50916 cached), out 1629.
Latency per item p50 4.5 s, p95 15.4 s.

**Run 2.** Items 9, no proposal 1 (failed 0, agent_limit 1); LLM − baseline -11.1 pp.
Cost $0.0000; per correct outcome $0 (local; hardware not modeled), 7447 tokens; tokens in 5612 (+52335 cached), out 1629.
Latency per item p50 4.5 s, p95 15.3 s.

**Run 3.** Items 9, no proposal 1 (failed 0, agent_limit 1); LLM − baseline -11.1 pp.
Cost $0.0000; per correct outcome $0 (local; hardware not modeled), 7447 tokens; tokens in 5612 (+52335 cached), out 1629.
Latency per item p50 4.4 s, p95 15.8 s.

## Misses

- run 1 HD-2012/refund_review: golden eligible; proposed none, final needs_human, baseline eligible
- run 1 HD-2021/refund_review: golden needs_human; proposed eligible, final needs_human, baseline needs_human
- run 2 HD-2012/refund_review: golden eligible; proposed none, final needs_human, baseline eligible
- run 2 HD-2021/refund_review: golden needs_human; proposed eligible, final needs_human, baseline needs_human
- run 3 HD-2012/refund_review: golden eligible; proposed none, final needs_human, baseline eligible
- run 3 HD-2021/refund_review: golden needs_human; proposed eligible, final needs_human, baseline needs_human
