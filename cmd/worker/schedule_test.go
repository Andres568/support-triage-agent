package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExec records calls; each call reports on started and, when gate is
// non-nil, blocks until gate is closed or ctx is done.
type fakeExec struct {
	mu      sync.Mutex
	calls   []string
	codes   map[string]int
	started chan string
	gate    chan struct{}
}

func (f *fakeExec) run(ctx context.Context, args []string) (int, error) {
	job := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, job)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- job
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	return f.codes[job], nil
}

// syncBuf is a log sink safe for the scheduler's goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestScheduler(f *fakeExec, skip bool) (*scheduler, *syncBuf) {
	logs := &syncBuf{}
	return &scheduler{
		jobs:     [][]string{{"-agent=support"}, {"-agent=orders"}},
		skipBusy: skip, exec: f.run,
		log: slog.New(slog.NewJSONHandler(logs, nil)),
	}, logs
}

// start runs the loop and returns its tick channel and a stop func that
// cancels and waits for the loop to return.
func start(s *scheduler) (chan time.Time, func()) {
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.loop(ctx, ticks); close(done) }()
	return ticks, func() { cancel(); <-done }
}

func TestSchedule_JobsRunInOrderEachTick(t *testing.T) {
	f := &fakeExec{started: make(chan string)}
	s, _ := newTestScheduler(f, false) // allow: the next tick may arrive before the busy slot is released
	ticks, stop := start(s)
	for range 2 {
		ticks <- time.Now()
		// Sequential: orders starts only after support returned.
		if a, b := <-f.started, <-f.started; a != "-agent=support" || b != "-agent=orders" {
			t.Fatalf("order = %q, %q", a, b)
		}
	}
	stop()
	want := []string{"-agent=support", "-agent=orders", "-agent=support", "-agent=orders"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestSchedule_SkipsTickWhilePreviousRuns(t *testing.T) {
	f := &fakeExec{started: make(chan string, 8), gate: make(chan struct{})}
	s, logs := newTestScheduler(f, true)
	ticks, stop := start(s)
	ticks <- time.Now()
	<-f.started // support is running and blocked
	ticks <- time.Now()
	ticks <- time.Now() // received only after the previous tick was handled
	close(f.gate)
	if j := <-f.started; j != "-agent=orders" {
		t.Fatalf("after the skipped ticks, started %q, want orders", j)
	}
	stop()

	if !slices.Equal(f.calls, []string{"-agent=support", "-agent=orders"}) {
		t.Errorf("calls = %v, want one tick's jobs", f.calls)
	}
	if n := strings.Count(logs.String(), "tick skipped"); n < 1 {
		t.Errorf("want a skipped tick logged, logs:\n%s", logs)
	}
}

func TestSchedule_AllowOverlapRunsConcurrently(t *testing.T) {
	f := &fakeExec{started: make(chan string, 8), gate: make(chan struct{})}
	s, _ := newTestScheduler(f, false)
	ticks, stop := start(s)
	ticks <- time.Now()
	ticks <- time.Now()
	<-f.started
	<-f.started // both ticks' support jobs are in flight at once
	close(f.gate)
	stop()
}

func TestSchedule_NonZeroExitLoggedAndNextJobRuns(t *testing.T) {
	f := &fakeExec{started: make(chan string), codes: map[string]int{"-agent=support": 1}}
	s, logs := newTestScheduler(f, false)
	ticks, stop := start(s)
	ticks <- time.Now()
	<-f.started
	<-f.started // orders still runs after support failed
	ticks <- time.Now()
	<-f.started // and the scheduler keeps ticking
	<-f.started
	stop()
	if !strings.Contains(logs.String(), `"msg":"job exited non-zero","args":"-agent=support","exit_code":1`) {
		t.Errorf("non-zero exit not logged:\n%s", logs)
	}
}

func TestSchedule_CancelStopsRunningJobAndLoop(t *testing.T) {
	f := &fakeExec{started: make(chan string, 8), gate: make(chan struct{})} // gate never closes
	s, logs := newTestScheduler(f, true)
	ticks, stop := start(s)
	ticks <- time.Now()
	<-f.started
	stop() // returns only once the in-flight job saw ctx and the loop exited

	if !slices.Equal(f.calls, []string{"-agent=support"}) {
		t.Errorf("calls = %v: no job may start after cancel", f.calls)
	}
	if !strings.Contains(logs.String(), context.Canceled.Error()) {
		t.Errorf("cancelled job not logged:\n%s", logs)
	}
}

func TestRunSchedule_RejectsBadFlags(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, args := range [][]string{{"-every=0s"}, {"-overlap=queue"}, {"-jobs= , "}} {
		if err := runSchedule(args, log); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
}

// runSchedule sets immediate: the first run starts without waiting a full
// interval.
func TestSchedule_ImmediateFiresBeforeFirstTick(t *testing.T) {
	f := &fakeExec{started: make(chan string, 2)}
	s, _ := newTestScheduler(f, true)
	s.immediate = true
	_, stop := start(s) // no tick is ever sent
	if got := <-f.started; got != "-agent=support" {
		t.Fatalf("first job = %q, want -agent=support", got)
	}
	<-f.started
	stop()
}

// -overlap=allow is bounded: past maxOverlappingTicks, ticks are skipped.
func TestSchedule_AllowOverlapIsCapped(t *testing.T) {
	f := &fakeExec{started: make(chan string, 16), gate: make(chan struct{})}
	s, logs := newTestScheduler(f, false)
	ticks, stop := start(s)
	for range maxOverlappingTicks + 2 {
		ticks <- time.Now()
	}
	for range maxOverlappingTicks {
		<-f.started
	}
	// The last receive may return before its fire() ran: wait for the log.
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), "tick skipped") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := strings.Count(logs.String(), "tick skipped"); n != 2 {
		t.Errorf("skipped ticks = %d, want 2", n)
	}
	stop()
}
