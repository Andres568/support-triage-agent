package queue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run describes one worker execution; its id owns every lease it takes.
type Run struct {
	Agent     string // "support" | "orders"
	Model     string // registry id, e.g. "ollama/qwen3:8b"
	Autonomy  string // "shadow" | "suggest" | "auto"
	PromptSHA string
}

// StartRun inserts a runs row and returns its id. Postgres generates the
// uuid, so the application needs no uuid library.
func StartRun(ctx context.Context, db *pgxpool.Pool, r Run) (RunID, error) {
	var id RunID
	err := db.QueryRow(ctx,
		`INSERT INTO runs (agent, model, autonomy, prompt_sha) VALUES ($1, $2, $3, $4) RETURNING id`,
		r.Agent, r.Model, r.Autonomy, r.PromptSHA).Scan(&id)
	if err != nil {
		return RunID{}, fmt.Errorf("start run: %w", err)
	}
	return id, nil
}

// RunStats counts what happened to the items a run claimed.
type RunStats struct {
	Claimed, Finalized, Abandoned, LostLeases int
}

// FinishRun closes a runs row: succeeded unless runErr is set. Item failures
// do not fail the run; they are counted, and recorded on the items.
func FinishRun(ctx context.Context, db *pgxpool.Pool, id RunID, s RunStats, runErr error) error {
	status, msg := "succeeded", (*string)(nil)
	if runErr != nil {
		status, msg = "failed", new(runErr.Error())
	}
	_, err := db.Exec(ctx, `
		UPDATE runs SET status = $2, claimed = $3, finalized = $4, abandoned = $5, lost_leases = $6,
		    error = $7, finished_at = now()
		WHERE id = $1`, id, status, s.Claimed, s.Finalized, s.Abandoned, s.LostLeases, msg)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}
