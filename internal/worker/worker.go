// Package worker runs one batch of a queue: claim an item, hand it to an
// agent's handler, and record a retry when the handler fails. It is agent
// agnostic; the support and orders agents plug in their own Handler.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

// leaseMargin is lease time left after the slowest item and its finalize,
// so clock skew and a slow commit cannot let another run reclaim a row that
// is still being written.
const leaseMargin = 15 * time.Second

var Agents = []string{"support", "orders"}

type Config struct {
	Agent       string
	Batch       int // items claimed per run, at most
	Concurrency int // items processed at the same time
	MaxAttempts int

	ItemTimeout     time.Duration // one item: model loop, tools, facts
	FinalizeTimeout time.Duration // the finalize or abandon write
	Lease           time.Duration
}

// Validate reports every problem at once.
func (c Config) Validate() error {
	var errs []error
	if !slices.Contains(Agents, c.Agent) {
		errs = append(errs, fmt.Errorf("agent %q is not one of %v", c.Agent, Agents))
	}
	if c.Batch < 1 || c.Concurrency < 1 || c.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("batch %d, concurrency %d and max attempts %d must be >= 1", c.Batch, c.Concurrency, c.MaxAttempts))
	}
	if c.ItemTimeout <= 0 || c.FinalizeTimeout <= 0 {
		errs = append(errs, fmt.Errorf("item timeout %v and finalize timeout %v must be > 0", c.ItemTimeout, c.FinalizeTimeout))
	}
	// Worst case under one lease: the item runs to its timeout, the handler's
	// finalize runs to its own, then the abandon write does too.
	// Recording the item (obs.ItemRecorder.Finish) comes after, outside the
	// lease: it never writes the leased row, and has its own short timeout.
	if need := c.ItemTimeout + 2*c.FinalizeTimeout + leaseMargin; c.Lease < need {
		errs = append(errs, fmt.Errorf("lease %v must be >= item timeout + 2 x finalize timeout + %v = %v", c.Lease, leaseMargin, need))
	}
	return errors.Join(errs...)
}

// Queue is what the runner needs from a store. Finalizing is deliberately not
// here: the handler finalizes through its store, which adds the outbox.
type Queue[T any] interface {
	Claim(ctx context.Context, run queue.RunID, limit int) ([]queue.Claimed[T], error)
	Abandon(ctx context.Context, l queue.Lease, cause error) error
	FailExhausted(ctx context.Context) (int64, error)
}

// Handler processes one claimed item and finalizes it itself. A returned
// error means "retry later": the runner abandons the lease. Outcomes that
// should not be retried (an agent that could not decide) must be finalized.
//
// The runner starts the item's span and recorder; Handle finds the recorder
// with obs.ItemFrom(ctx) and reports its decision with Decided.
type Handler[T any] interface {
	Handle(ctx context.Context, c queue.Claimed[T]) error
	// Subject names an item for its span and run row: its kind (obs.Kind*)
	// and a readable id.
	Subject(c queue.Claimed[T]) (kind, subject string)
}

// Stats is what a run records in its runs row.
type Stats = queue.RunStats

// Detached returns a context for a write that must complete even if ctx was
// cancelled or its deadline passed: the item's work is done and only the
// write is left. It keeps ctx's values (trace ids) but not its cancellation.
func Detached(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// Run processes up to cfg.Batch items, then returns. When ctx is cancelled
// (SIGTERM) it stops claiming, but items already claimed run to completion
// under their own timeout: stopping them halfway would only waste the work,
// and the lease protects the row if the process is killed anyway.
//
// The returned error covers infrastructure failures of the run itself
// (claiming). Item failures are counted in Stats and recorded on the rows.
// of records each item's span and run row; nil records nothing.
func Run[T any](ctx context.Context, cfg Config, q Queue[T], run queue.RunID, h Handler[T], of *obs.Factory, log *slog.Logger) (Stats, error) {
	if err := cfg.Validate(); err != nil {
		return Stats{}, err
	}
	log = log.With("agent", cfg.Agent, "run", run.String())

	// Rows whose last attempt crashed stay claimed forever unless closed.
	if n, err := q.FailExhausted(ctx); err != nil {
		return Stats{}, err
	} else if n > 0 {
		log.Warn("failed exhausted items", "count", n)
	}

	var (
		mu       sync.Mutex
		stats    Stats
		errs     []error
		reserved atomic.Int64 // claim slots taken, so the batch is never exceeded
		wg       sync.WaitGroup
	)
	count := func(f func(*Stats)) { mu.Lock(); f(&stats); mu.Unlock() }

	for range cfg.Concurrency {
		wg.Go(func() {
			for ctx.Err() == nil && reserved.Add(1) <= int64(cfg.Batch) {
				// One at a time: a lease starts only when its work starts.
				claimed, err := q.Claim(ctx, run, 1)
				if err != nil {
					if ctx.Err() == nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
					return
				}
				if len(claimed) == 0 {
					return
				}
				count(func(s *Stats) { s.Claimed++ })
				process(ctx, cfg, q, h, claimed[0], of, log, count)
			}
		})
	}
	wg.Wait()

	if ctx.Err() != nil {
		log.Info("stopped claiming", "cause", context.Cause(ctx))
	}
	return stats, errors.Join(errs...)
}

func process[T any](ctx context.Context, cfg Config, q Queue[T], h Handler[T], c queue.Claimed[T], of *obs.Factory, log *slog.Logger, count func(func(*Stats))) {
	log = log.With("id", c.ID, "attempt", c.Attempt)
	start := time.Now()

	// The item's span and recorder live here, not in the handler, so the
	// recorded outcome is the real one (finalized, abandoned, lost) and a
	// panicking handler still ends its span and gets a row. Finish runs
	// after the finalize or abandon, outside the lease budget: it writes only
	// run_items and run_steps, never the leased row.
	kind, subject := h.Subject(c)
	spanCtx, ir := of.Item(ctx, c.Run, kind, c.ID, c.Attempt, subject)
	outcome, err := obs.OutcomeAbandoned, error(nil)
	defer func() { ir.Finish(spanCtx, outcome, err) }()

	// Detached from ctx: a shutdown signal stops claiming, not this item.
	itemCtx, cancel := Detached(spanCtx, cfg.ItemTimeout)
	err = handle(itemCtx, h, c, log)
	cancel()

	// Logged with spanCtx, so each line carries the item's trace and span.
	latency := time.Since(start).Milliseconds()
	switch {
	case err == nil:
		outcome = obs.OutcomeFinalized
		count(func(s *Stats) { s.Finalized++ })
		log.InfoContext(spanCtx, "item finalized", "latency_ms", latency)
	case errors.Is(err, queue.ErrLostLease):
		outcome = obs.OutcomeLostLease
		count(func(s *Stats) { s.LostLeases++ })
		log.WarnContext(spanCtx, "lease lost; result discarded", "latency_ms", latency, "error.type", ErrorType(err))
	default:
		abandonCtx, cancel := Detached(spanCtx, cfg.FinalizeTimeout)
		defer cancel()
		switch aerr := q.Abandon(abandonCtx, c.Lease, err); {
		case aerr == nil:
			count(func(s *Stats) { s.Abandoned++ })
			log.WarnContext(spanCtx, "item abandoned for retry", "latency_ms", latency, "error.type", ErrorType(err))
		case errors.Is(aerr, queue.ErrLostLease):
			outcome = obs.OutcomeLostLease
			count(func(s *Stats) { s.LostLeases++ })
			log.WarnContext(spanCtx, "lease lost while abandoning", "latency_ms", latency, "error.type", ErrorType(err))
		default:
			// The lease will expire and the row be retried anyway, so it is
			// abandoned in effect; counting it keeps claimed = the sum.
			outcome = obs.OutcomeAbandonFailed
			count(func(s *Stats) { s.Abandoned++ })
			log.ErrorContext(spanCtx, "abandon failed", "latency_ms", latency, "error.type", ErrorType(err), "abandon_error.type", ErrorType(aerr))
		}
	}
}

// handle turns a panic into an error, so one bad item is retried instead of
// killing the whole batch.
func handle[T any](ctx context.Context, h Handler[T], c queue.Claimed[T], log *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// The stack goes to the log only: last_error is stored and shown.
			log.ErrorContext(ctx, "handler panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h.Handle(ctx, c)
}

// knownErrors are sentinels whose text is ours and safe to log.
var knownErrors = []error{context.DeadlineExceeded, context.Canceled, queue.ErrLostLease}

// ErrorType is an error's class for logs (OpenTelemetry's error.type): a
// known sentinel's text, else the Go type of the innermost wrapped error.
// Never the message: error texts can quote model output, customer text or
// upstream response bodies. The full text is kept only in the database
// (last_error, capped), where access is controlled.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	for _, k := range knownErrors {
		if errors.Is(err, k) {
			return k.Error()
		}
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}
