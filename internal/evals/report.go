package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
)

// Summary is summary.json: one agent, one model, every repeat.
type Summary struct {
	Agent     string   `json:"agent"`
	Model     string   `json:"model"`
	Mode      string   `json:"mode"` // live | replay
	PromptSHA string   `json:"prompt_sha"`
	Autonomy  string   `json:"autonomy"`
	Cases     []string `json:"cases"`
	Runs      []Scores `json:"runs"`
	// Mean, Min and Max of each rate over the runs where it applies.
	// Promotion needs a metric to hold on every repeat: Min for rates where
	// higher is better, Max for the others.
	Mean map[string]float64 `json:"mean"`
	Min  map[string]float64 `json:"min"`
	Max  map[string]float64 `json:"max"`
}

// Aggregate fills Mean, Min and Max from Runs.
func (s *Summary) Aggregate() {
	s.Mean, s.Min, s.Max = map[string]float64{}, map[string]float64{}, map[string]float64{}
	n := map[string]int{}
	for _, r := range s.Runs {
		for name, m := range r.Rates {
			if m.N == 0 {
				continue
			}
			if n[name] == 0 {
				s.Min[name], s.Max[name] = m.Rate, m.Rate
			}
			n[name]++
			s.Mean[name] += m.Rate
			s.Min[name], s.Max[name] = min(s.Min[name], m.Rate), max(s.Max[name], m.Rate)
		}
	}
	for name := range s.Mean {
		s.Mean[name] = round(s.Mean[name] / float64(n[name]))
	}
}

// SameOutcome compares two summaries ignoring what a replay cannot
// reproduce: latency and the mode.
func SameOutcome(a, b Summary) bool {
	strip := func(s Summary) Summary {
		s.Mode = ""
		s.Runs = slices.Clone(s.Runs)
		for i := range s.Runs {
			s.Runs[i].Latency = nil
		}
		return s
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

func equalScores(a, b Scores) bool {
	a.Latency, b.Latency = nil, nil
	return reflect.DeepEqual(a, b)
}

func ReadSummary(path string) (Summary, error) {
	var s Summary
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

// Thresholds is evals/thresholds.json, per agent. Max and Min hold for every
// repeat. A null value is a placeholder: a regression floor not measured
// yet, which is not checked.
type Thresholds struct {
	Note   string                     `json:"note,omitempty"`
	Agents map[string]AgentThresholds `json:"agents"`
}

type AgentThresholds struct {
	Max map[string]*float64 `json:"max"` // hard safety invariants, e.g. false auto-replies
	Min map[string]*float64 `json:"min"`
}

func ReadThresholds(path string) (Thresholds, error) {
	var t Thresholds
	raw, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return t, dec.Decode(&t)
}

// Check lists every threshold a run of s breaks. A metric that does not
// apply to a run (n = 0, e.g. no injection case selected) is skipped; an
// unknown metric name is a problem, so a typo cannot disable a check.
func (t Thresholds) Check(s Summary) []string {
	at, ok := t.Agents[s.Agent]
	if !ok {
		return []string{fmt.Sprintf("no thresholds for agent %q", s.Agent)}
	}
	var out []string
	check := func(kind string, limits map[string]*float64, fails func(rate, limit float64) bool) {
		for _, name := range slices.Sorted(maps.Keys(limits)) {
			limit := limits[name]
			for i, r := range s.Runs {
				m, known := r.Rates[name]
				switch {
				case !known:
					out = append(out, fmt.Sprintf("%s %s: unknown metric", kind, name))
				case limit == nil || m.N == 0:
				case fails(m.Rate, *limit):
					out = append(out, fmt.Sprintf("run %d: %s = %.4f (%d/%d), %s %.4f", i+1, name, m.Rate, m.K, m.N, kind, *limit))
				}
			}
		}
	}
	check("max", at.Max, func(rate, limit float64) bool { return rate > limit })
	check("min", at.Min, func(rate, limit float64) bool { return rate < limit })
	return out
}

// reportRows are the metrics in the report, in order, with a note.
var reportRows = map[string][][2]string{
	"support": {
		{"category_accuracy", "model-run tickets"},
		{"action_accuracy_proposed", "model output vs golden"},
		{"action_accuracy_final", "after the gate vs policy-capped golden"},
		{"escalation_recall", "golden escalate → escalated"},
		{"over_escalation_rate", "lower is better"},
		{"false_auto_reply_rate_proposed", "what the model asked for; tickets with a proposal (see no_proposal)"},
		{"false_auto_reply_rate_final", "**must be 0**"},
		{"injection_pass_rate", "**must be 1**"},
		{"follow_up_precision", "informational"},
		{"follow_up_recall", "informational"},
	},
	"orders": {
		{"verdict_accuracy_proposed", "model output vs golden"},
		{"verdict_accuracy_final", "after the gate"},
		{"baseline_accuracy", "rules only, no LLM"},
		{"unsafe_eligible_rate_proposed", "eligible where golden is not; tasks with a proposal (see no_proposal)"},
		{"unsafe_eligible_rate_final", "0 by construction"},
	},
}

// Markdown renders report.md: metrics with CIs per run, counts, cost,
// latency, and every item that missed its label.
func Markdown(s Summary, rs []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Eval: %s agent, %s\n\n", s.Agent, s.Model)
	fmt.Fprintf(&b, "Mode %s, autonomy %s, %d item(s), %d run(s), prompt %.12s.\n\n", s.Mode, s.Autonomy, len(s.Cases), len(s.Runs), s.PromptSHA)
	b.WriteString("| Metric |")
	for i := range s.Runs {
		fmt.Fprintf(&b, " Run %d (95%% CI) |", i+1)
	}
	b.WriteString(" Mean | Min | Max | Note |\n|---|" + strings.Repeat("---|", len(s.Runs)+4) + "\n")
	for _, row := range reportRows[s.Agent] {
		name := row[0]
		fmt.Fprintf(&b, "| %s |", name)
		for _, run := range s.Runs {
			b.WriteString(" " + cell(run.Rates[name]) + " |")
		}
		if mean, ok := s.Mean[name]; ok {
			fmt.Fprintf(&b, " %.3f | %.3f | %.3f | %s |\n", mean, s.Min[name], s.Max[name], row[1])
		} else {
			fmt.Fprintf(&b, " n/a | n/a | n/a | %s |\n", row[1])
		}
	}
	for i, run := range s.Runs {
		c := run.Counts
		fmt.Fprintf(&b, "\n**Run %d.** Items %d, no proposal %d (failed %d, agent_limit %d)", i+1, c["items"], c["no_proposal"], c["failed"], c["agent_limit"])
		if s.Agent == "support" {
			fmt.Fprintf(&b, ", pre-checked %d; the gate prevented %d false auto-reply(ies)", c["precheck"], c["gate_prevented_false_auto_replies"])
		} else if acc, base := run.Rates["verdict_accuracy_final"], run.Rates["baseline_accuracy"]; acc.N > 0 {
			fmt.Fprintf(&b, "; LLM − baseline %+.1f pp", 100*(acc.Rate-base.Rate))
		}
		b.WriteString(".\n")
		fmt.Fprintf(&b, "Cost %s; per correct outcome %s", usd(c["cost_micros"]), perCorrect(c))
		fmt.Fprintf(&b, "; tokens in %d (+%d cached), out %d.\n", c["input_tokens"], c["cache_read_tokens"], c["output_tokens"])
		if run.Latency != nil {
			fmt.Fprintf(&b, "Latency per item p50 %.1f s, p95 %.1f s.\n", float64(run.Latency.P50MS)/1000, float64(run.Latency.P95MS)/1000)
		} else {
			b.WriteString("Latency n/a (replayed).\n")
		}
	}
	b.WriteString("\n## Misses\n\n")
	misses := 0
	for i, res := range rs {
		for _, t := range res.Tickets {
			if !t.Missed() {
				continue
			}
			misses++
			fmt.Fprintf(&b, "- run %d %s: golden %s/%s, expected final %s; %s\n", i+1, t.ID, t.GoldenCategory, t.GoldenAction, t.ExpectedFinal, describeTicket(t))
		}
		for _, t := range res.Tasks {
			if t.Err == "" && t.Final == t.Golden && t.Proposed != nil && *t.Proposed == t.Golden {
				continue
			}
			misses++
			proposed := "none"
			if t.Proposed != nil {
				proposed = string(*t.Proposed)
			}
			fmt.Fprintf(&b, "- run %d %s: golden %s; proposed %s, final %s, baseline %s%s\n", i+1, t.ID, t.Golden, proposed, t.Final, t.Baseline, errSuffix(t.Err))
		}
	}
	if misses == 0 {
		b.WriteString("None.\n")
	}
	return b.String()
}

func cell(m Metric) string {
	if m.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d = %.3f [%.2f, %.2f]", m.K, m.N, m.Rate, m.Lo, m.Hi)
}

func describeTicket(t TicketResult) string {
	if t.Final == nil || t.Err != "" {
		return "not decided" + errSuffix(t.Err)
	}
	s := fmt.Sprintf("source %s, final %s/%s", t.Source, t.Final.Category, t.Final.Action)
	if t.Proposed != nil {
		s += fmt.Sprintf(", proposed %s/%s", t.Proposed.Category, t.Proposed.Action)
	}
	return s
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " (" + e + ")"
}

func usd(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

// perCorrect is the cost per correct outcome; for a free local model it
// shows tokens instead, since the hardware is not modeled.
func perCorrect(c map[string]int64) string {
	if c["correct"] == 0 {
		return "n/a (nothing correct)"
	}
	if c["cost_micros"] == 0 {
		return fmt.Sprintf("$0 (local; hardware not modeled), %d tokens", c["tokens_per_correct"])
	}
	return usd(c["cost_per_correct_micros"])
}

// CompareTable renders one markdown table per agent, models as columns. A
// cell is the mean over the repeats and the worst repeat with its Wilson 95%
// interval: a criterion must hold on every repeat, so the worst one is what
// promotion reads.
func CompareTable(ss []Summary) string {
	var b strings.Builder
	for _, agentName := range []string{"support", "orders"} {
		var cols []Summary
		for _, s := range ss {
			if s.Agent == agentName {
				cols = append(cols, s)
			}
		}
		if len(cols) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n| Metric |", agentName)
		for _, s := range cols {
			fmt.Fprintf(&b, " %s (%s, %d runs) |", s.Model, s.Mode, len(s.Runs))
		}
		b.WriteString("\n|---|" + strings.Repeat("---|", len(cols)) + "\n")
		for _, row := range reportRows[agentName] {
			fmt.Fprintf(&b, "| %s |", row[0])
			for _, s := range cols {
				b.WriteString(" " + compareCell(s, row[0]) + " |")
			}
			b.WriteString("\n")
		}
		if agentName == "orders" {
			b.WriteString("| LLM − baseline (final, mean) |")
			for _, s := range cols {
				fmt.Fprintf(&b, " %+.3f |", s.Mean["verdict_accuracy_final"]-s.Mean["baseline_accuracy"])
			}
			b.WriteString("\n")
		}
		for _, count := range []string{"no_proposal", "agent_limit", "failed", "correct"} {
			fmt.Fprintf(&b, "| %s (per run) |", count)
			for _, s := range cols {
				var vs []string
				for _, r := range s.Runs {
					vs = append(vs, fmt.Sprint(r.Counts[count]))
				}
				b.WriteString(" " + strings.Join(vs, ", ") + " |")
			}
			b.WriteString("\n")
		}
		b.WriteString("| p95 latency (worst run) |")
		for _, s := range cols {
			var p95 int64
			for _, r := range s.Runs {
				if r.Latency != nil {
					p95 = max(p95, r.Latency.P95MS)
				}
			}
			fmt.Fprintf(&b, " %.1f s |", float64(p95)/1000)
		}
		b.WriteString("\n| Cost per correct (run 1) |")
		for _, s := range cols {
			cost := "–"
			if len(s.Runs) > 0 {
				cost = perCorrect(s.Runs[0].Counts)
			}
			b.WriteString(" " + cost + " |")
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// compareCell is "mean; worst k/n [lo, hi]", worst by the rate's direction.
func compareCell(s Summary, name string) string {
	lower := LowerIsBetter(name)
	var worst *Metric
	for _, r := range s.Runs {
		m, ok := r.Rates[name]
		if !ok || m.N == 0 {
			continue
		}
		if worst == nil || (lower && m.Rate > worst.Rate) || (!lower && m.Rate < worst.Rate) {
			worst = &m
		}
	}
	if worst == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f; worst %d/%d [%.2f, %.2f]", s.Mean[name], worst.K, worst.N, worst.Lo, worst.Hi)
}
