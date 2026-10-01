package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/providers/httpjson"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

// Subject kinds, as stored in run_items.subject_kind.
const (
	KindTicket = "ticket"
	KindTask   = "task"
)

// Outcomes besides an item's final status.
const (
	// OutcomeFinalized stands for the status the handler reported with
	// Decided; Finish stores that status instead.
	OutcomeFinalized     = "finalized"
	OutcomeAbandoned     = "abandoned"
	OutcomeLostLease     = "lost_lease"
	OutcomeAbandonFailed = "abandon_failed"
	// OutcomeFinalizedUnknown is stored when the worker reports
	// OutcomeFinalized but the handler never called Decided: the item was
	// finalized with a status the recorder does not know. A handler bug,
	// recorded as an error rather than guessed.
	OutcomeFinalizedUnknown = "finalized_unknown"
)

// errMissingDecided marks an item finalized without Decided.
var errMissingDecided = errors.New("finalized without Decided")

// Size caps of what a run_items row stores.
const (
	// MaxTranscriptBytes caps a stored transcript: above it the column is
	// NULL and transcript_truncated is set. A loop that big is a runaway, and
	// its text is customer data we would rather not keep in bulk.
	MaxTranscriptBytes = 64 << 10
	// MaxErrorBytes caps stored error text. The database may hold error
	// text: it already holds the ticket, under the same access and
	// retention. Spans may not: they leave for a trace backend with neither.
	// The cap keeps one quoted response body from bloating a row.
	MaxErrorBytes = 1 << 10
)

// Beginner opens a transaction; *pgxpool.Pool is one.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Factory makes one ItemRecorder per claimed item. A nil *Factory records
// nothing, so handlers work without observability (tests, evals).
type Factory struct {
	DB     Beginner            // nil: spans only, no rows
	Spec   providers.ModelSpec // the run's model: span names, provider, price
	Tracer trace.TracerProvider
	Log    *slog.Logger // where a failed row write is reported; nil: slog.Default()
	// WriteTimeout bounds the rows' write, which is detached from the
	// item's cancellation; zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
}

// DefaultWriteTimeout is short on purpose: the write runs after the item's
// lease work is done, and a slow telemetry write must not stall a batch.
const DefaultWriteTimeout = 2 * time.Second

type itemKey struct{}

// Item starts the item's "process {kind}" span and returns its context,
// which also carries the recorder (see ItemFrom). Pass that context to the
// handler so model and tool spans nest under it.
func (f *Factory) Item(ctx context.Context, run queue.RunID, kind string, id int64, attempt int, subject string) (context.Context, *ItemRecorder) {
	if f == nil {
		return ctx, nil
	}
	tp := f.Tracer
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	r := &ItemRecorder{f: f, tracer: tp.Tracer(TracerName), run: run, kind: kind, id: id, attempt: attempt, start: time.Now()}
	// The kind, not the subject, names the span: span names must be low
	// cardinality. The subject is an attribute.
	ctx, r.span = r.tracer.Start(ctx, "process "+kind,
		trace.WithAttributes(AppSubject.String(subject), AppAttempt.Int(attempt)))
	return context.WithValue(ctx, itemKey{}, r), r
}

// ItemFrom returns the recorder of the item ctx belongs to, or nil.
func ItemFrom(ctx context.Context) *ItemRecorder {
	r, _ := ctx.Value(itemKey{}).(*ItemRecorder)
	return r
}

// Step is one model call or tool call of an item.
type Step struct {
	Kind         string // "model" | "tool"
	Name         string // model id or tool name
	Start        time.Time
	Latency      time.Duration
	Usage        agent.Usage // model steps only
	CostMicros   int64       // model steps only
	FinishReason string
	Err          error
}

// ItemRecorder records one item's steps as spans and, on Finish, as rows.
// All methods are no-ops on a nil receiver.
type ItemRecorder struct {
	f       *Factory
	tracer  trace.Tracer
	span    trace.Span
	run     queue.RunID
	kind    string
	id      int64
	attempt int
	start   time.Time

	mu      sync.Mutex
	steps   []Step
	pending []agent.ToolCall // the last response's calls, to find a tool call's id
	result  *agent.Result
	status  string // from Decided
	source  string

	finish sync.Once
}

// Model decorates m: each call gets a "chat {model}" span and a step.
func (r *ItemRecorder) Model(m agent.Model) agent.Model {
	if r == nil {
		return m
	}
	return recordedModel{m: m, r: r}
}

// Tools decorates ts: each call gets an "execute_tool {name}" span and a step.
func (r *ItemRecorder) Tools(ts []agent.Tool) []agent.Tool {
	if r == nil {
		return ts
	}
	out := make([]agent.Tool, len(ts))
	for i, t := range ts {
		out[i] = recordedTool{t: t, r: r}
	}
	return out
}

// Loop keeps the loop's result for the item row (steps, transcript).
func (r *ItemRecorder) Loop(res agent.Result) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.result = &res
	r.mu.Unlock()
}

// Decided reports the status the handler is about to finalize and the
// decision's source. Call it before the finalize: whether the finalize
// succeeded is the worker's to report, through Finish.
func (r *ItemRecorder) Decided(status, source string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.status, r.source = status, source
	r.mu.Unlock()
}

// Steps returns a copy of the steps recorded so far; nil on a nil receiver.
func (r *ItemRecorder) Steps() []Step {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.steps)
}

// Usage is the item's total usage and its cost, rounded once; zero on a nil
// receiver.
func (r *ItemRecorder) Usage() (agent.Usage, int64) {
	var u agent.Usage
	if r == nil {
		return u, 0
	}
	for _, s := range r.Steps() {
		u.Add(s.Usage)
	}
	return u, r.f.Spec.Price.Cost(u)
}

// Finish ends the item's span and writes its run_items and run_steps rows,
// once: later calls do nothing. The worker calls it after the finalize or
// the abandon, with the real outcome; OutcomeFinalized becomes the status
// reported with Decided. The write keeps ctx's trace but not its
// cancellation, bounded by WriteTimeout. It is best-effort: a failure is
// logged, never returned, so telemetry can never fail or retry a decision.
func (r *ItemRecorder) Finish(ctx context.Context, outcome string, err error) {
	if r == nil {
		return
	}
	r.finish.Do(func() { r.doFinish(ctx, outcome, err) })
}

func (r *ItemRecorder) doFinish(ctx context.Context, outcome string, err error) {
	defer r.span.End()
	r.mu.Lock()
	if outcome == OutcomeFinalized {
		if r.status != "" {
			outcome = r.status
		} else {
			outcome, err = OutcomeFinalizedUnknown, errors.Join(errMissingDecided, err)
		}
	}
	source := r.source
	r.mu.Unlock()
	if outcome == OutcomeFinalizedUnknown {
		r.logger().WarnContext(ctx, "item finalized without Decided", "kind", r.kind, "id", r.id, "attempt", r.attempt)
	}

	r.span.SetAttributes(AppOutcome.String(outcome))
	if err != nil {
		r.span.SetAttributes(ErrorType.String(errorType(err)))
		r.span.SetStatus(codes.Error, errorType(err))
	}
	if r.f.DB == nil {
		return
	}
	timeout := r.f.WriteTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if werr := r.write(ctx, outcome, source, err); werr != nil {
		r.logger().ErrorContext(ctx, "run item write failed", "kind", r.kind, "id", r.id, "attempt", r.attempt, "err", werr)
	}
}

func (r *ItemRecorder) logger() *slog.Logger {
	if r.f.Log != nil {
		return r.f.Log
	}
	return slog.Default()
}

func (r *ItemRecorder) write(ctx context.Context, outcome, source string, itemErr error) error {
	steps := r.Steps()
	usage, cost := r.Usage()
	modelSteps := 0
	for _, s := range steps {
		if s.Kind == "model" {
			modelSteps++
		}
	}
	var transcript []byte
	r.mu.Lock()
	if r.result != nil {
		var err error
		if transcript, err = json.Marshal(r.result.Transcript); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	r.mu.Unlock()
	truncated := len(transcript) > MaxTranscriptBytes
	if truncated {
		transcript = nil
	}

	return pgx.BeginFunc(ctx, r.f.DB, func(tx pgx.Tx) error {
		var itemID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO run_items (run_id, subject_kind, subject_id, attempt, outcome, source, latency_ms,
			    cost_micros, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, steps, error,
			    transcript, transcript_truncated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			RETURNING id`,
			r.run, r.kind, r.id, r.attempt, outcome, nullString(source), time.Since(r.start).Milliseconds(),
			cost, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens, modelSteps,
			errText(itemErr), transcript, truncated).Scan(&itemID)
		if err != nil {
			return err
		}
		b := &pgx.Batch{}
		for i, s := range steps {
			var in, out, cr, cw, c *int64
			if s.Kind == "model" {
				in, out = new(int64(s.Usage.InputTokens)), new(int64(s.Usage.OutputTokens))
				cr, cw, c = new(int64(s.Usage.CacheReadTokens)), new(int64(s.Usage.CacheWriteTokens)), new(s.CostMicros)
			}
			b.Queue(`
				INSERT INTO run_steps (run_item_id, seq, kind, name, started_at, latency_ms, input_tokens,
				    output_tokens, cache_read_tokens, cache_write_tokens, cost_micros, finish_reason, is_error, error)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
				itemID, i+1, s.Kind, s.Name, s.Start, s.Latency.Milliseconds(), in, out, cr, cw, c,
				nullString(s.FinishReason), s.Err != nil, errText(s.Err))
		}
		return tx.SendBatch(ctx, b).Close()
	})
}

func (r *ItemRecorder) add(s Step) {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
}

// callID finds the id of the pending call a tool is running, by name and
// arguments: agent.Tool.Call does not receive it. Each id is handed out
// once, so identical calls in one turn get their own ids, in order.
func (r *ItemRecorder) callID(name string, args json.RawMessage) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.pending {
		if c.Name == name && bytes.Equal(c.Args, args) {
			r.pending = slices.Delete(r.pending, i, i+1)
			return c.ID
		}
	}
	return ""
}

type recordedModel struct {
	m agent.Model
	r *ItemRecorder
}

func (m recordedModel) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	spec := m.r.f.Spec
	model := spec.APIModel
	if model == "" {
		model = spec.ID
	}
	ctx, span := m.r.tracer.Start(ctx, OpChat+" "+model, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(GenAIOperationName.String(OpChat), GenAIProviderName.String(string(spec.Provider)),
			GenAIRequestModel.String(model)))
	defer span.End()

	start := time.Now()
	resp, err := m.m.Generate(ctx, req)
	u := resp.Usage
	cost := spec.Price.Cost(u)
	m.r.add(Step{Kind: "model", Name: spec.ID, Start: start, Latency: time.Since(start),
		Usage: u, CostMicros: cost, FinishReason: resp.StopReason, Err: err})

	// Only when reported: a call that returned no usage is unknown, not free.
	if in := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens; in != 0 || u.OutputTokens != 0 {
		span.SetAttributes(GenAIUsageInput.Int(in), GenAIUsageOutput.Int(u.OutputTokens), AppCostMicros.Int64(cost))
	}
	if resp.Model != "" {
		span.SetAttributes(GenAIResponseModel.String(resp.Model))
	}
	if resp.ID != "" {
		span.SetAttributes(GenAIResponseID.String(resp.ID))
	}
	if resp.StopReason != "" {
		span.SetAttributes(GenAIFinishReasons.StringSlice([]string{resp.StopReason}))
	}
	if err != nil {
		span.SetAttributes(ErrorType.String(errorType(err)))
		span.SetStatus(codes.Error, errorType(err))
		return resp, err
	}
	m.r.mu.Lock()
	m.r.pending = slices.Clone(resp.Message.ToolCalls)
	m.r.mu.Unlock()
	return resp, nil
}

type recordedTool struct {
	t agent.Tool
	r *ItemRecorder
}

func (t recordedTool) Spec() agent.ToolSpec { return t.t.Spec() }

func (t recordedTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	name := t.t.Spec().Name
	attrs := []attribute.KeyValue{GenAIOperationName.String(OpExecuteTool), GenAIToolName.String(name)}
	if id := t.r.callID(name, args); id != "" {
		attrs = append(attrs, GenAIToolCallID.String(id))
	}
	ctx, span := t.r.tracer.Start(ctx, OpExecuteTool+" "+name, trace.WithAttributes(attrs...))
	defer span.End()

	start := time.Now()
	out, err := t.t.Call(ctx, args)
	t.r.add(Step{Kind: "tool", Name: name, Start: start, Latency: time.Since(start), Err: err})
	if err != nil {
		span.SetAttributes(ErrorType.String(errorType(err)))
		span.SetStatus(codes.Error, errorType(err))
	}
	return out, err
}

// errorType is a low-cardinality error.type that never carries message
// text: provider and tool errors can quote customer data.
func errorType(err error) string {
	var he *httpjson.HTTPError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, queue.ErrLostLease):
		return "lost_lease"
	case errors.Is(err, agent.ErrTruncated):
		return "truncated"
	case errors.Is(err, agent.ErrRefused):
		return "refused"
	case errors.Is(err, agent.ErrContextWindow):
		return "context_window"
	case errors.Is(err, agent.ErrMaxSteps):
		return "max_steps"
	case errors.Is(err, agent.ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.As(err, &he):
		return strconv.Itoa(he.Status)
	case errors.Is(err, errMissingDecided):
		return OutcomeFinalizedUnknown
	}
	return "_OTHER"
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// errText is err's message capped at MaxErrorBytes, cut on a rune boundary
// so Postgres accepts it.
func errText(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	if len(s) > MaxErrorBytes {
		s = strings.ToValidUTF8(s[:MaxErrorBytes], "")
	}
	return &s
}
