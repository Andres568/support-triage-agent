package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
)

// echo is a tool whose output depends on the day, like business days in the
// real tools: replay must not care.
type echo struct{ day string }

func (e echo) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (e echo) Call(_ context.Context, args json.RawMessage) (string, error) {
	return e.day + ":" + string(args), nil
}

func config(day string) agent.Config {
	return agent.Config{
		System:   "test",
		Tools:    []agent.Tool{echo{day}},
		MaxSteps: 5,
		Finish:   agent.ToolSpec{Name: "finish", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validate: func(json.RawMessage) error { return nil },
	}
}

func script() *agenttest.Script {
	return agenttest.NewScript(
		agenttest.Text("thinking out loud"),
		agenttest.Call("c1", "echo", map[string]string{"q": "a"}),
		agenttest.Call("c2", "finish", map[string]string{"answer": "done"}),
	)
}

func TestRecordThenReplay_SameResult(t *testing.T) {
	ctx := context.Background()
	c := New("fake/script", "p1", "t1")
	key := Key("HD-2001", 1)
	recorded, err := agent.Run(ctx, NewRecorder(c, script(), key), config("monday"), "ticket")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, "fake/script", "p1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Steps(key); len(got) != 3 || !reflect.DeepEqual(got[2].RequestSummary, []string{"echo"}) {
		t.Fatalf("entries = %+v, want 3 steps, the last after one echo call", got)
	}

	// Another day: tool output differs, so the requests differ too.
	replayed, err := agent.Run(ctx, NewReplayer(loaded, key), config("tuesday"), "ticket")
	if err != nil {
		t.Fatal(err)
	}
	// The cassette is indented for readable diffs, which re-indents raw JSON
	// args: compare them as values.
	if !jsonEqual(replayed.Final, recorded.Final) || replayed.Steps != recorded.Steps || replayed.Usage != recorded.Usage {
		t.Errorf("replayed %+v, recorded %+v", replayed, recorded)
	}
	if len(replayed.Transcript) != len(recorded.Transcript) {
		t.Fatalf("transcript lengths %d, %d", len(replayed.Transcript), len(recorded.Transcript))
	}
	for i, m := range recorded.Transcript {
		if m.Role != agent.RoleAssistant {
			continue // user turns hold today's tool output
		}
		r := replayed.Transcript[i]
		if r.Text != m.Text || len(r.ToolCalls) != len(m.ToolCalls) {
			t.Fatalf("turn %d: replayed %+v, recorded %+v", i, r, m)
		}
		for j, c := range m.ToolCalls {
			if rc := r.ToolCalls[j]; rc.ID != c.ID || rc.Name != c.Name || !jsonEqual(rc.Args, c.Args) {
				t.Errorf("turn %d call %d: replayed %+v, recorded %+v", i, j, rc, c)
			}
		}
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// failingModel fails the way an adapter does.
type failingModel struct{ err error }

func (f failingModel) Generate(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{Usage: agent.Usage{InputTokens: 7}}, f.err
}

func TestReplay_Errors(t *testing.T) {
	ctx := context.Background()
	c := New("m", "p", "t")
	if _, err := NewRecorder(c, script(), "A#1").Generate(ctx, agent.Request{}); err != nil {
		t.Fatal(err)
	}

	r := NewReplayer(c, "A#1")
	if _, err := r.Generate(ctx, agent.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Generate(ctx, agent.Request{}); !errors.Is(err, ErrCassetteExhausted) {
		t.Errorf("err = %v, want ErrCassetteExhausted", err)
	}
	if _, err := NewReplayer(c, "B#1").Generate(ctx, agent.Request{}); !errors.Is(err, ErrCassetteMiss) {
		t.Errorf("err = %v, want ErrCassetteMiss", err)
	}

	// A model outcome is replayed as the same sentinel; an outage is not recorded.
	truncated := fmt.Errorf("anthropic: %w", agent.ErrTruncated)
	if _, err := NewRecorder(c, failingModel{truncated}, "C#1").Generate(ctx, agent.Request{}); !errors.Is(err, agent.ErrTruncated) {
		t.Fatalf("recorder err = %v", err)
	}
	resp, err := NewReplayer(c, "C#1").Generate(ctx, agent.Request{})
	if !errors.Is(err, agent.ErrTruncated) || resp.Usage.InputTokens != 7 {
		t.Errorf("replayed resp = %+v, err = %v; want ErrTruncated with usage", resp, err)
	}
	// An outage is replayed as a retryable outage, not as drift.
	_, _ = NewRecorder(c, failingModel{errors.New("http 503")}, "D#1").Generate(ctx, agent.Request{})
	if _, err := NewReplayer(c, "D#1").Generate(ctx, agent.Request{}); !errors.Is(err, ErrRecordedOutage) || errors.Is(err, ErrCassetteExhausted) {
		t.Errorf("replayed outage err = %v, want ErrRecordedOutage", err)
	}
	// Our own cancellation is not recorded at all.
	_, _ = NewRecorder(c, failingModel{context.Canceled}, "E#1").Generate(ctx, agent.Request{})
	if len(c.Steps("E#1")) != 0 {
		t.Error("a cancellation was recorded")
	}

	path := filepath.Join(t.TempDir(), "c.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("saved file: %v, %v; want mode 0644", fi, err)
	}
	if _, err := Load(path, "other", "p", "t"); !errors.Is(err, ErrWrongModel) {
		t.Errorf("Load(other model) err = %v, want ErrWrongModel", err)
	}
	for _, shas := range [][2]string{{"p2", "t"}, {"p", "t2"}} {
		if _, err := Load(path, "m", shas[0], shas[1]); !errors.Is(err, ErrStaleCassette) {
			t.Errorf("Load(%v) err = %v, want ErrStaleCassette", shas, err)
		}
	}
}

func TestRecorder_ReRecordReplacesSteps(t *testing.T) {
	c := New("m", "p", "t")
	for range 2 {
		if _, err := NewRecorder(c, script(), "A#1").Generate(context.Background(), agent.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.Steps("A#1")); n != 1 {
		t.Errorf("steps = %d, want 1", n)
	}
}

// Items run concurrently in the worker; run with -race.
func TestRecorder_Concurrent(t *testing.T) {
	c := New("m", "p", "t")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := Key(fmt.Sprintf("HD-%d", i), 1)
			if _, err := agent.Run(context.Background(), NewRecorder(c, script(), key), config("monday"), "t"); err != nil {
				t.Error(err)
			}
			if _, err := agent.Run(context.Background(), NewReplayer(c, key), config("monday"), "t"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(c.Keys()) != 20 {
		t.Errorf("keys = %d, want 20", len(c.Keys()))
	}
	if err := c.Save(filepath.Join(t.TempDir(), "c.json")); err != nil {
		t.Fatal(err)
	}
}

func TestReplay_DivergedAndDone(t *testing.T) {
	ctx := context.Background()
	c := New("fake/script", "p", "t")
	key := Key("HD-2001", 1)
	if _, err := agent.Run(ctx, NewRecorder(c, script(), key), config("monday"), "ticket"); err != nil {
		t.Fatal(err)
	}

	// Same input on another day: every step matches and is consumed.
	r := NewReplayer(c, key)
	if _, err := agent.Run(ctx, r, config("tuesday"), "ticket"); err != nil {
		t.Fatal(err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done = %v", err)
	}

	// Another input: the first step already diverges, and Done names it.
	r = NewReplayer(c, key)
	if _, err := agent.Run(ctx, r, config("monday"), "another ticket"); !errors.Is(err, ErrReplayDiverged) {
		t.Fatalf("err = %v, want ErrReplayDiverged", err)
	}
	if err := r.Done(); !errors.Is(err, ErrReplayDiverged) || !strings.Contains(err.Error(), key+" step 1") {
		t.Errorf("Done = %v, want divergence naming %s step 1", err, key)
	}

	// A tool that now fails changes the next request: diverged at step 3.
	failing := config("monday")
	failing.Tools = []agent.Tool{brokenEcho{}}
	r = NewReplayer(c, key)
	if _, err := agent.Run(ctx, r, failing, "ticket"); !errors.Is(err, ErrReplayDiverged) || !strings.Contains(err.Error(), "step 3") {
		t.Fatalf("err = %v, want ErrReplayDiverged at step 3", err)
	}

	// Steps left over: the replay stopped early.
	r = NewReplayer(c, key)
	if _, err := r.Generate(ctx, agent.Request{Messages: []agent.Message{{Role: agent.RoleUser, Text: "ticket"}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Done(); !errors.Is(err, ErrReplayDiverged) || !strings.Contains(err.Error(), "used 1 of 3") {
		t.Errorf("Done = %v, want unconsumed steps", err)
	}
}

type brokenEcho struct{ echo }

func (brokenEcho) Call(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("echo is down")
}
