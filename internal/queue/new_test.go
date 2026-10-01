package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/queue"
)

// No database needed: these fail before any query.

func TestNew_RejectsMisconfiguration(t *testing.T) {
	tests := []struct {
		name        string
		table, col  string
		lease       time.Duration
		maxAttempts int
	}{
		{"unknown table", "users", "decision", time.Minute, 3},
		{"result column of another table", "tickets", "proposal", time.Minute, 3},
		{"zero lease", "tickets", "decision", 0, 3},
		{"no attempts", "tasks", "proposal", time.Minute, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			queue.New(nil, queue.Spec[string]{Table: tt.table, ResultColumn: tt.col}, tt.lease, tt.maxAttempts)
		})
	}
}

func TestFinalize_RejectsNonFinalStatus(t *testing.T) {
	q := queue.New(nil, spec, time.Minute, maxAttempts)
	for _, status := range []string{"pending", "claimed"} {
		if err := q.Finalize(context.Background(), queue.Lease{ID: 1}, status, done, nil); !errors.Is(err, queue.ErrNotFinal) {
			t.Errorf("Finalize(%q) = %v, want ErrNotFinal", status, err)
		}
	}
}
