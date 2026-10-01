package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// `worker schedule -every=2m -jobs=support,orders` is the local stand-in for
// Cloud Scheduler firing Cloud Run Jobs. Each tick runs the jobs in order,
// each as a fresh subprocess of this binary (`worker -agent=<job>`), like a
// Cloud Run Job execution. No docker socket, no third-party image.
//
// Cloud Scheduler does not skip a fire while the previous execution runs;
// -overlap=skip does, locally. Either way a double fire is harmless: the
// claim/fencing in internal/queue lets two runs share a queue without
// finalizing an item twice (the queue concurrency test proves it).

// execFunc runs one job to completion and returns its exit code; err is for
// failures to start or wait, not for a non-zero exit.
type execFunc func(ctx context.Context, args []string) (int, error)

type scheduler struct {
	jobs      [][]string // args per job, run in order each tick
	skipBusy  bool       // -overlap=skip
	immediate bool       // fire once at start, before the first tick
	exec      execFunc
	log       *slog.Logger
}

func runSchedule(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	every := fs.Duration("every", 2*time.Minute, "interval between ticks")
	jobs := fs.String("jobs", "support,orders", "agents to run in order each tick")
	overlap := fs.String("overlap", "skip", "skip: drop a tick while the previous one runs; allow: run up to 4 ticks concurrently")
	grace := fs.Duration("grace", 30*time.Second, "after SIGTERM, how long a job may finish before it is killed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *every <= 0 {
		return fmt.Errorf("-every %v must be > 0", *every)
	}
	if *overlap != "skip" && *overlap != "allow" {
		return fmt.Errorf("-overlap %q must be skip or allow", *overlap)
	}
	s := &scheduler{skipBusy: *overlap == "skip", immediate: true, exec: subprocess(*grace), log: log}
	for j := range strings.SplitSeq(*jobs, ",") {
		if j = strings.TrimSpace(j); j != "" {
			s.jobs = append(s.jobs, []string{"-agent=" + j})
		}
	}
	if len(s.jobs) == 0 {
		return errors.New("-jobs is empty")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := time.NewTicker(*every)
	defer t.Stop()
	log.Info("scheduler started", "every", every.String(), "jobs", *jobs, "overlap", *overlap)
	s.loop(ctx, t.C)
	log.Info("scheduler stopped")
	return nil
}

// maxOverlappingTicks caps -overlap=allow: a job that hangs must not grow
// goroutines and subprocesses without bound. Past it, ticks are skipped as
// with -overlap=skip.
const maxOverlappingTicks = 4

// loop fires on every tick until ctx is done, then waits for running jobs
// (which the exec func has already been asked to stop).
func (s *scheduler) loop(ctx context.Context, ticks <-chan time.Time) {
	var wg sync.WaitGroup
	slots := 1
	if !s.skipBusy {
		slots = maxOverlappingTicks
	}
	busy := make(chan struct{}, slots)
	fire := func() {
		select {
		case busy <- struct{}{}:
		default:
			s.log.Warn("tick skipped: previous ticks still running", "running", slots)
			return
		}
		wg.Go(func() {
			defer func() { <-busy }()
			s.tick(ctx)
		})
	}
	if s.immediate {
		fire()
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticks:
			fire()
		}
	}
}

// tick runs the jobs in order. A failing job is logged and the next one
// still runs: the orders agent drains tasks left by earlier runs too.
func (s *scheduler) tick(ctx context.Context) {
	for _, args := range s.jobs {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		code, err := s.exec(ctx, args)
		attrs := []any{"args", strings.Join(args, " "), "exit_code", code, "duration_ms", time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			s.log.Error("job failed to run", append(attrs, "err", err)...)
		case code != 0:
			s.log.Error("job exited non-zero", attrs...)
		default:
			s.log.Info("job finished", attrs...)
		}
	}
}

// subprocess runs this binary with args, inheriting the environment and
// output. Cancelling ctx sends SIGTERM (what Cloud Run sends), then kills
// after grace.
func subprocess(grace time.Duration) execFunc {
	return func(ctx context.Context, args []string) (int, error) {
		self, err := os.Executable()
		if err != nil {
			return -1, err
		}
		cmd := exec.CommandContext(ctx, self, args...)
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = grace
		err = cmd.Run()
		// After a SIGTERM, Run reports ctx's error even when the job exited
		// cleanly; the exit status is what matters.
		if ps := cmd.ProcessState; ps != nil && ps.Exited() {
			return ps.ExitCode(), nil
		}
		return -1, err
	}
}
