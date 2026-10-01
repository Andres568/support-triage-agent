package tools

import (
	"context"
	"errors"
	"sync"

	"github.com/Andres568/support-triage-agent/internal/commerce"
)

// Watched is a Commerce that remembers what the tools did: which orders they
// fetched, and the first infrastructure error. The loop shows tool errors to
// the model; handlers need them to tell a retry from a decision.
type Watched struct {
	Commerce
	mu     sync.Mutex
	orders []string
	err    error
}

func NewWatched(c Commerce) *Watched { return &Watched{Commerce: c} }

func (w *Watched) Order(ctx context.Context, num, email string) (commerce.Order, error) {
	o, err := w.Commerce.Order(ctx, num, email)
	w.record(err, func() { w.orders = append(w.orders, o.OrderNumber) })
	return o, err
}

func (w *Watched) SearchPolicies(ctx context.Context, q string) ([]commerce.Policy, error) {
	ps, err := w.Commerce.SearchPolicies(ctx, q)
	w.record(err, func() {})
	return ps, err
}

func (w *Watched) record(err error, ok func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case err == nil:
		ok()
	// Not found and rejected are answers about the input, not outages.
	case !errors.Is(err, commerce.ErrNotFound) && !errors.Is(err, commerce.ErrRejected) && w.err == nil:
		w.err = err
	}
}

// Failure is the first infrastructure error a tool got, if any.
func (w *Watched) Failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Fetched lists the orders the tools fetched successfully.
func (w *Watched) Fetched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.orders...)
}
