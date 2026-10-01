package evals

import (
	"fmt"
	"slices"

	"github.com/Andres568/support-triage-agent/internal/triage"
)

// OracleDeviations are the items where a perfect model still does not end
// at the golden label, because the gate is right to differ. Each needs a
// reason; anything else the oracle gets wrong is a pipeline bug.
var OracleDeviations = map[string]string{}

// OracleProblems checks an oracle run item by item: every ticket ends at
// its expected final action with exactly the golden follow-ups, every task
// at its golden verdict, nothing fails or hits a limit, and no replay is
// needed to say so.
func OracleProblems(r Result, gs []Golden, gts []GoldenTask, p triage.Policy) []string {
	var out []string
	bad := func(id, format string, args ...any) {
		if _, ok := OracleDeviations[id]; !ok {
			out = append(out, id+": "+fmt.Sprintf(format, args...))
		}
	}
	byTicket := map[string]TicketResult{}
	for _, t := range r.Tickets {
		byTicket[t.ID] = t
	}
	for _, g := range gs {
		t, ok := byTicket[g.ExternalID]
		switch {
		case !ok || t.Err != "" || t.Final == nil:
			bad(g.ExternalID, "not decided: %s", t.Err)
		case t.Source == triage.SourceAgentLimit:
			bad(g.ExternalID, "agent_limit: %s", t.Final.Reason)
		case t.Final.Action != ExpectedFinal(g, p):
			bad(g.ExternalID, "final %s, expected %s", t.Final.Action, ExpectedFinal(g, p))
		case t.Source == triage.SourceAgent && t.Final.Category != g.Category:
			bad(g.ExternalID, "category %s, golden %s", t.Final.Category, g.Category)
		case !slices.Equal(t.Final.FollowUps, g.FollowUps): // nil and [] are equal
			bad(g.ExternalID, "follow-ups %v, golden %v", t.Final.FollowUps, g.FollowUps)
		}
	}
	byTask := map[string]TaskResult{}
	for _, t := range r.Tasks {
		byTask[t.ID] = t
	}
	for _, g := range gts {
		t, ok := byTask[g.Subject()]
		switch {
		case !ok || t.Err != "":
			bad(g.Subject(), "not proposed: %s", t.Err)
		case t.Final != g.Verdict || t.Proposed == nil || *t.Proposed != g.Verdict:
			bad(g.Subject(), "final %s, golden %s", t.Final, g.Verdict)
		}
	}
	return out
}
