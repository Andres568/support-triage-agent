package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/evals"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func TestSlug(t *testing.T) {
	if got := slug("ollama/qwen3:8b"); got != "ollama-qwen3-8b" {
		t.Errorf("slug = %q", got)
	}
	if got := slug("grok-4.7"); got != "grok-4-7" {
		t.Errorf("slug = %q", got)
	}
}

func TestParse_Rejects(t *testing.T) {
	for _, args := range [][]string{
		{"-agent", "billing"},
		{"-record", "a.json", "-replay", "b.json"},
		{"-replay", "b.json", "-repeat", "3"},
		{"-repeat", "0"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("parse(%v) accepted", args)
		}
	}
}

func TestCompare(t *testing.T) {
	dir := t.TempDir()
	s := evals.Summary{Agent: "support", Model: "fake/oracle", Mode: "live",
		Runs: []evals.Scores{{Rates: map[string]evals.Metric{"category_accuracy": evals.NewMetric(9, 10)}}}}
	s.Aggregate()
	raw, _ := json.Marshal(s)
	path := filepath.Join(dir, "summary.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := compare(&out, []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "| category_accuracy | 0.900; worst 9/10 [0.60, 0.98] |") {
		t.Errorf("table:\n%s", out.String())
	}
}

// TestCompareReplay: the replay gate fails closed. A missing reference is an
// error, and every differing, extra or missing item is a problem.
func TestCompareReplay(t *testing.T) {
	ticket := func(id string, a triage.Action) evals.TicketResult {
		return evals.TicketResult{ID: id, Source: triage.SourceAgent, Final: &triage.Decision{Category: triage.CategoryGeneral, Action: a}}
	}
	recorded := evals.Result{Tickets: []evals.TicketResult{ticket("HD-1", triage.ActionAutoReply), ticket("HD-2", triage.ActionEscalate)}}
	s := evals.Summary{Agent: "support", Model: "fake/oracle", Mode: "replay"}
	writeRef := func(t *testing.T, summary, items bool) string {
		dir := t.TempDir()
		cassette := filepath.Join(dir, "c.json")
		if summary {
			raw, _ := json.Marshal(s)
			if err := os.WriteFile(summaryPath(cassette), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if items {
			raw, _ := json.Marshal(evals.Outcomes(recorded))
			if err := os.WriteFile(itemsPath(cassette), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return cassette
	}
	differs := evals.Result{Tickets: []evals.TicketResult{ticket("HD-1", triage.ActionAutoReply), ticket("HD-2", triage.ActionDraftForReview)}}
	missing := evals.Result{Tickets: recorded.Tickets[:1]}
	extra := evals.Result{Tickets: append(slices.Clone(recorded.Tickets), ticket("HD-3", triage.ActionEscalate))}
	tests := []struct {
		name           string
		summary, items bool
		got            evals.Result
		wantErr        bool
		want           []string // substrings, one per problem
	}{
		{"reproduced", true, true, recorded, false, nil},
		{"no summary", false, true, recorded, true, nil},
		{"no items", true, false, recorded, true, nil},
		{"one item differs", true, true, differs, false, []string{"replay item HD-2"}},
		{"missing item", true, true, missing, false, []string{"replay item HD-2", "replay has 1 items, recording 2"}},
		{"extra item", true, true, extra, false, []string{"replay has 3 items, recording 2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, err := compareReplay(writeRef(t, tt.summary, tt.items), s, tt.got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if len(problems) != len(tt.want) {
				t.Fatalf("problems %q, want %q", problems, tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(problems[i], w) {
					t.Errorf("problem %d = %q, want %q", i, problems[i], w)
				}
			}
		})
	}
	if p, _ := compareReplay(writeRef(t, true, true), evals.Summary{Agent: "orders"}, recorded); len(p) != 1 {
		t.Errorf("a different summary: problems %q, want 1", p)
	}
}

func TestOptions_CasesTypoWithLimit(t *testing.T) {
	t.Chdir("../..")
	if _, err := options(flags{agent: "support", model: evals.OracleModel, autonomy: "auto", cases: "HD-2001,HD-9999", limit: 1}, nil); err == nil {
		t.Error("a typo in -cases passed because -limit dropped it")
	}
	if _, err := options(flags{agent: "support", model: evals.OracleModel, autonomy: "auto", cases: "HD-2001,HD-2002", limit: 1}, nil); err != nil {
		t.Errorf("-limit dropping a valid case: %v", err)
	}
}
