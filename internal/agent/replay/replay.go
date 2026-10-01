// Package replay records and replays model responses at the neutral
// agent.Model level, so evals and demos run offline and deterministically.
//
// Calls are matched by (subject, attempt, step), not by a hash of the
// request: tool results contain day-dependent business days, so a request
// hash would never match on another day. Instead each step stores a
// day-independent fingerprint of its request (see fingerprint), and a replay
// that sends a different request fails as diverged rather than answering a
// conversation that never happened. Staleness of the whole cassette is caught
// by the model id and the prompt and tools SHA stored with it.
//
// Record only against the synthetic eval seed (the triage_eval database),
// never against production tickets: a cassette holds the model's replies,
// which quote customer text and order details, and it is committed to git.
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
)

var (
	// ErrCassetteMiss and ErrCassetteExhausted mean the replay drifted from
	// the recording: an item or a step the recording never saw.
	ErrCassetteMiss      = errors.New("replay: no recording for this item")
	ErrCassetteExhausted = errors.New("replay: recording has no more steps")
	// ErrReplayDiverged means the request of a step differs from the recorded
	// one (another input, another tool error, another nudge): the code under
	// replay no longer behaves as it did when recorded.
	ErrReplayDiverged = errors.New("replay: request diverged from the recording")
	// ErrRecordedOutage replays a provider outage (HTTP 5xx, timeout) seen
	// while recording. It is retryable like the original, so the item ends the
	// same way; it says nothing about drift.
	ErrRecordedOutage = errors.New("replay: recorded provider outage")
	ErrStaleCassette  = errors.New("replay: recording is stale; re-record with make eval-record (needs Ollama)")
	ErrWrongModel     = errors.New("replay: recording is of another model")
)

// Cassette is one recorded eval run. It is safe for concurrent use: the
// worker runs items in parallel, each through its own recorder or replayer,
// and the entries are only reachable through locked methods.
type Cassette struct {
	Model      string
	PromptSHA  string
	ToolsSHA   string
	RecordedAt time.Time // set by Save
	// Fixture is the caller's own state the recording depends on (evals:
	// the pinned clock and the seeded timestamps), restored before a replay.
	Fixture json.RawMessage

	mu      sync.Mutex
	entries map[string][]Entry // by Key(subject, attempt), one entry per step
}

// wireCassette is the file format.
type wireCassette struct {
	Model      string             `json:"model"`
	PromptSHA  string             `json:"prompt_sha"`
	ToolsSHA   string             `json:"tools_sha"`
	RecordedAt time.Time          `json:"recorded_at"`
	Fixture    json.RawMessage    `json:"fixture,omitempty"`
	Entries    map[string][]Entry `json:"entries"`
}

// Entry is one model call. RequestSummary lists the tool calls made so far,
// so a diff between two recordings shows where the runs diverged.
type Entry struct {
	Response       agent.Response `json:"response"`
	Error          string         `json:"error,omitempty"`
	ErrorKind      string         `json:"error_kind,omitempty"` // a neutral sentinel, see errorKinds, or transient
	LatencyMS      int64          `json:"latency_ms"`
	RequestSummary []string       `json:"request_summary"`
	Fingerprint    string         `json:"fingerprint"`
}

// errorKinds are the provider outcomes worth replaying: they decide how an
// item ends (escalated as agent_limit), so a replay must end the same way.
var errorKinds = map[string]error{
	"truncated":      agent.ErrTruncated,
	"refused":        agent.ErrRefused,
	"context_window": agent.ErrContextWindow,
}

// kindTransient is any other provider error: an outage. It is recorded with
// its message only and replayed as ErrRecordedOutage.
const kindTransient = "transient"

// Key identifies one attempt at one item, e.g. "HD-2001#1".
func Key(subject string, attempt int) string {
	return subject + "#" + strconv.Itoa(attempt)
}

func New(model, promptSHA, toolsSHA string) *Cassette {
	return &Cassette{Model: model, PromptSHA: promptSHA, ToolsSHA: toolsSHA, entries: map[string][]Entry{}}
}

// Load reads a cassette and refuses it when it is of another model, or the
// prompt or tools changed since it was recorded: its answers would describe
// another agent.
func Load(path, model, promptSHA, toolsSHA string) (*Cassette, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	var w wireCassette
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	if w.Model != model {
		return nil, fmt.Errorf("%w (%s: recorded %q, requested %q)", ErrWrongModel, path, w.Model, model)
	}
	if w.PromptSHA != promptSHA || w.ToolsSHA != toolsSHA {
		return nil, fmt.Errorf("%w (%s: prompt %.12s, tools %.12s; current prompt %.12s, tools %.12s)",
			ErrStaleCassette, path, w.PromptSHA, w.ToolsSHA, promptSHA, toolsSHA)
	}
	c := &Cassette{Model: w.Model, PromptSHA: w.PromptSHA, ToolsSHA: w.ToolsSHA, RecordedAt: w.RecordedAt, Fixture: w.Fixture, entries: w.Entries}
	if c.entries == nil {
		c.entries = map[string][]Entry{}
	}
	return c, nil
}

// Steps returns a copy of the recorded steps for key.
func (c *Cassette) Steps(key string) []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.entries[key])
}

// Keys returns every recorded key, sorted.
func (c *Cassette) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Save writes the cassette atomically (sync, then rename), so an interrupted
// recording never leaves a half-written file behind. RecordedAt is set here.
func (c *Cassette) Save(path string) error {
	c.mu.Lock()
	c.RecordedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(wireCassette{c.Model, c.PromptSHA, c.ToolsSHA, c.RecordedAt, c.Fixture, c.entries}, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("replay: encode: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return errors.Join(fmt.Errorf("replay: %w", err), tmp.Close())
	}
	// CreateTemp makes the file 0600; a committed cassette is ordinary data.
	if err := tmp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("replay: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("replay: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	return nil
}

type recorder struct {
	c     *Cassette
	inner agent.Model
	key   string
}

// NewRecorder passes calls to inner and records each one under key. It
// starts the key afresh, so re-recording an item replaces the old steps.
func NewRecorder(c *Cassette, inner agent.Model, key string) agent.Model {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
	return &recorder{c: c, inner: inner, key: key}
}

func (r *recorder) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	start := time.Now()
	resp, err := r.inner.Generate(ctx, req)
	e := Entry{Response: resp, LatencyMS: time.Since(start).Milliseconds(), RequestSummary: summary(req), Fingerprint: fingerprint(req)}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Our own shutdown, not the provider: nothing to replay.
			return resp, err
		}
		e.Error, e.ErrorKind = err.Error(), errorKind(err)
	}
	r.c.mu.Lock()
	r.c.entries[r.key] = append(r.c.entries[r.key], e)
	r.c.mu.Unlock()
	return resp, err
}

func errorKind(err error) string {
	for kind, sentinel := range errorKinds {
		if errors.Is(err, sentinel) {
			return kind
		}
	}
	return kindTransient
}

// summary is the name of every tool called so far, in order.
func summary(req agent.Request) []string {
	names := []string{}
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			names = append(names, c.Name)
		}
	}
	return names
}

// fingerprint is a hash of what in a request does not depend on the day: the
// number of messages, and for each user turn its text (the input, a nudge)
// and which tool results were errors. Tool result content is left out: it
// holds business days that change daily. Assistant turns are left out too:
// under replay they are the recording's own answers.
func fingerprint(req agent.Request) string {
	b := fmt.Appendf(nil, "messages=%d\n", len(req.Messages))
	for i, m := range req.Messages {
		if m.Role != agent.RoleUser {
			continue
		}
		b = fmt.Appendf(b, "%d text=%x errors=", i, sha256.Sum256([]byte(m.Text)))
		for _, r := range m.ToolResults {
			b = fmt.Appendf(b, "%t,", r.IsError)
		}
		b = append(b, '\n')
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Replayer answers each call with the next recorded step for its key.
type Replayer struct {
	c   *Cassette
	key string

	mu   sync.Mutex
	step int
	err  error // the first drift error, sticky
}

func NewReplayer(c *Cassette, key string) *Replayer {
	return &Replayer{c: c, key: key}
}

func (r *Replayer) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	entries := r.c.Steps(r.key)
	r.mu.Lock()
	defer r.mu.Unlock()
	step := r.step
	r.step++
	var e Entry
	switch {
	case len(entries) == 0:
		return agent.Response{}, r.drift(fmt.Errorf("%w: %s", ErrCassetteMiss, r.key))
	case step >= len(entries):
		return agent.Response{}, r.drift(fmt.Errorf("%w: %s has %d steps", ErrCassetteExhausted, r.key, len(entries)))
	default:
		e = entries[step]
	}
	if got := fingerprint(req); got != e.Fingerprint {
		return agent.Response{}, r.drift(fmt.Errorf("%w: %s step %d (%d messages; recorded after tools %v)",
			ErrReplayDiverged, r.key, step+1, len(req.Messages), e.RequestSummary))
	}
	switch e.ErrorKind {
	case "":
		return e.Response, nil
	case kindTransient:
		return e.Response, fmt.Errorf("%w: %s step %d (recorded: %s)", ErrRecordedOutage, r.key, step+1, e.Error)
	}
	sentinel, known := errorKinds[e.ErrorKind]
	if !known {
		return agent.Response{}, r.drift(fmt.Errorf("replay: %s step %d: unknown error kind %q", r.key, step+1, e.ErrorKind))
	}
	return e.Response, fmt.Errorf("replay: %w (recorded: %s)", sentinel, e.Error)
}

// drift keeps the first drift error for Done. r.mu is held.
func (r *Replayer) drift(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// Done reports whether the replay matched the recording: no drift, and every
// recorded step was consumed. Call it after the item has finished.
func (r *Replayer) Done() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if n := len(r.c.Steps(r.key)); r.step < n {
		return fmt.Errorf("%w: %s used %d of %d recorded steps", ErrReplayDiverged, r.key, r.step, n)
	}
	return nil
}
