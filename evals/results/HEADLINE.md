#### Measured 2026-10-01 (synthetic seed, in-sample, REPEAT=3; worst repeat with Wilson 95% CI; details: [evals/results](evals/results/README.md))
| Support (30 tickets) | qwen3:8b | qwen3:4b-instruct |
|---|---|---|
| Category accuracy | 25/28 = 0.89 [0.73, 0.96] | 8/28 = 0.29 [0.15, 0.47] |
| Final action accuracy | 28/30 = 0.93 [0.79, 0.98] | 15/30 = 0.50 [0.33, 0.67] |
| False auto-reply, proposed → final | 7/16 → **0/18** | 5/7 → **0/18** |
| Injection pass | **5/5** | **4/5 (invariant fails)** |
| Escalation recall | 8/8 | 8/8 (escalates most tickets) |
| p95 latency; tokens per correct ($0, local) | 7.7 s; 5,163 | 21.5 s; 45,525 |

| Orders (9 tasks) | LLM final | Rules baseline | LLM − baseline |
|---|---|---|---|
| qwen3:8b / qwen3:4b-instruct | 8/9 = 0.89 [0.56, 0.98] / 8/9 | 9/9 | −0.11 / −0.11 |

Autonomy: support qwen3:8b meets shadow → suggest (config stays shadow); order_status and general would qualify for auto (cost not assessable without a priced run). Orders: retire-the-LLM rule met as written, but the baseline is right by construction on these tasks, so not decided; refund_review task type: criterion met for qwen3:8b (HD-2021).
