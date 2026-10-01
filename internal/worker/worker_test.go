package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func config() Config {
	return Config{Agent: "support", Batch: 100, Concurrency: 3, MaxAttempts: 3,
		ItemTimeout: time.Second, FinalizeTimeout: time.Second, Lease: time.Minute}
}

// fakeQueue hands out n items and records what the runner did with them.
type fakeQueue struct {
	mu        sync.Mutex
	next, n   int
	abandoned []error
	failCalls int
	claimErr  error
}

func (q *fakeQueue) Claim(_ context.Context, run queue.RunID, limit int) ([]queue.Claimed[int], error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	if limit != 1 {
		panic("runner must claim one item at a time")
	}
	if q.next >= q.n {
		return nil, nil
	}
	q.next++
	return []queue.Claimed[int]{{Lease: queue.Lease{ID: int64(q.next), Run: run, Attempt: 1}, Item: q.next}}, nil
}

func (q *fakeQueue) Abandon(_ context.Context, _ queue.Lease, cause error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.abandoned = append(q.abandoned, cause)
	return nil
}

func (q *fakeQueue) FailExhausted(context.Context) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failCalls++
	return 0, nil
}

type handlerFunc func(ctx context.Context, c queue.Claimed[int]) error

func (f handlerFunc) Handle(ctx context.Context, c queue.Claimed[int]) error { return f(ctx, c) }
func (handlerFunc) Subject(c queue.Claimed[int]) (string, string) {
	return obs.KindTask, strconv.Itoa(c.Item)
}

func TestRun_OutcomesAreCounted(t *testing.T) {
	q := &fakeQueue{n: 6}
	h := handlerFunc(func(_ context.Context, c queue.Claimed[int]) error {
		switch c.Item % 3 {
		case 1:
			return nil
		case 2:
			return errors.New("model timeout")
		}
		if c.Item == 6 {
			panic("boom")
		}
		return queue.ErrLostLease
	})
	stats, err := Run(context.Background(), config(), q, queue.RunID{}, h, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Claimed: 6, Finalized: 2, Abandoned: 3, LostLeases: 1} // 2, 5 fail; 6 panics
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if q.failCalls != 1 {
		t.Errorf("FailExhausted called %d times, want once", q.failCalls)
	}
	if !slicesContainsSubstring(q.abandoned, "handler panic: boom") {
		t.Errorf("abandon causes = %v, want the panic recorded", q.abandoned)
	}
}

func slicesContainsSubstring(errs []error, s string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), s) {
			return true
		}
	}
	return false
}

func TestRun_StopsAtBatchWithBoundedConcurrency(t *testing.T) {
	q := &fakeQueue{n: 50}
	cfg := config()
	cfg.Batch, cfg.Concurrency = 7, 3
	var inFlight, peak atomic.Int64
	h := handlerFunc(func(context.Context, queue.Claimed[int]) error {
		n := inFlight.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	stats, err := Run(context.Background(), cfg, q, queue.RunID{}, h, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 7 || stats.Finalized != 7 {
		t.Errorf("stats = %+v, want exactly the batch of 7", stats)
	}
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak concurrency = %d, want 2..3", p)
	}
}

// A shutdown signal stops claiming; the item in flight still completes and
// is not cancelled by the signal.
func TestRun_CancelStopsClaimingButFinishesInFlight(t *testing.T) {
	q := &fakeQueue{n: 10}
	cfg := config()
	cfg.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	h := handlerFunc(func(itemCtx context.Context, _ queue.Claimed[int]) error {
		cancel() // SIGTERM arrives while the first item runs
		time.Sleep(10 * time.Millisecond)
		return itemCtx.Err() // nil: the item's context ignores the signal
	})
	stats, err := Run(ctx, cfg, q, queue.RunID{}, h, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{Claimed: 1, Finalized: 1}) {
		t.Errorf("stats = %+v, want one claimed and finalized item", stats)
	}
}

func TestRun_ItemTimeout(t *testing.T) {
	q := &fakeQueue{n: 1}
	cfg := config()
	cfg.ItemTimeout = 10 * time.Millisecond
	h := handlerFunc(func(ctx context.Context, _ queue.Claimed[int]) error {
		<-ctx.Done()
		return ctx.Err()
	})
	stats, err := Run(context.Background(), cfg, q, queue.RunID{}, h, nil, quiet)
	if err != nil || stats.Abandoned != 1 || !errors.Is(q.abandoned[0], context.DeadlineExceeded) {
		t.Fatalf("stats = %+v, err %v, abandoned %v; want the timed-out item abandoned", stats, err, q.abandoned)
	}
}

func TestRun_ClaimErrorFailsTheRun(t *testing.T) {
	q := &fakeQueue{n: 1, claimErr: errors.New("connection refused")}
	if _, err := Run(context.Background(), config(), q, queue.RunID{}, handlerFunc(nil), nil, quiet); err == nil {
		t.Fatal("Run = nil, want the claim error")
	}
}

func TestConfig_Validate(t *testing.T) {
	if err := config().Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	tests := map[string]func(*Config){
		"unknown agent":            func(c *Config) { c.Agent = "billing" },
		"zero batch":               func(c *Config) { c.Batch = 0 },
		"zero concurrency":         func(c *Config) { c.Concurrency = 0 },
		"no attempts":              func(c *Config) { c.MaxAttempts = 0 },
		"no item timeout":          func(c *Config) { c.ItemTimeout = 0 },
		"lease shorter than items": func(c *Config) { c.Lease = c.ItemTimeout + c.FinalizeTimeout + 14*time.Second },
	}
	for name, mutate := range tests {
		c := config()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
}

// The runner owns each item's span: it records the real outcome, including
// for a panic, and nests the handler's work under it.
func TestRun_RecordsOutcomesOnItemSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	of := &obs.Factory{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))}
	q := &fakeQueue{n: 4}
	cfg := config()
	cfg.Concurrency = 1
	h := handlerFunc(func(ctx context.Context, c queue.Claimed[int]) error {
		switch c.Item {
		case 1:
			obs.ItemFrom(ctx).Decided("drafted", "agent")
			return nil
		case 2:
			return queue.ErrLostLease
		case 4:
			return nil // finalized, but the handler forgot Decided
		}
		panic("boom")
	})
	if _, err := Run(context.Background(), cfg, q, queue.RunID{}, h, of, quiet); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"1": "drafted", "2": obs.OutcomeLostLease, "3": obs.OutcomeAbandoned,
		"4": obs.OutcomeFinalizedUnknown}
	ended := sr.Ended()
	if len(ended) != len(want) {
		t.Fatalf("%d spans ended, want %d", len(ended), len(want))
	}
	for _, s := range ended {
		set := attribute.NewSet(s.Attributes()...)
		subject, _ := set.Value(obs.AppSubject)
		outcome, _ := set.Value(obs.AppOutcome)
		if s.Name() != "process task" || outcome.AsString() != want[subject.AsString()] {
			t.Errorf("span %q subject %s: outcome %q, want %q", s.Name(), subject.AsString(), outcome.AsString(), want[subject.AsString()])
		}
		if subject.AsString() == "3" && s.Status().Description != "_OTHER" {
			t.Errorf("panic span status = %+v, want an error without its message", s.Status())
		}
		if subject.AsString() == "4" && s.Status().Description != obs.OutcomeFinalizedUnknown {
			t.Errorf("span without Decided: status = %+v, want an error", s.Status())
		}
	}
}

type secretErr struct{}

func (secretErr) Error() string { return "customer wrote: IGNORE ALL RULES" }

func TestErrorType_NeverTheMessage(t *testing.T) {
	for err, want := range map[error]string{
		nil: "",
		fmt.Errorf("item: %w", context.DeadlineExceeded): "context deadline exceeded",
		fmt.Errorf("finalize: %w", queue.ErrLostLease):   queue.ErrLostLease.Error(),
		fmt.Errorf("call: %w", secretErr{}):              "worker.secretErr",
	} {
		if got := ErrorType(err); got != want {
			t.Errorf("ErrorType(%v) = %q, want %q", err, got, want)
		}
	}
}
