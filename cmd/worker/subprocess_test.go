package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The subprocess tests re-run this test binary as the "job" (os.Executable
// is the test binary here). helperEnv selects what the child does; the child
// never reaches the test framework, so job args like -agent=x are ignored.
const (
	helperEnv   = "SCHEDULE_TEST_HELPER"
	helperReady = "SCHEDULE_TEST_READY" // file the child creates once its handler is set
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "exit3":
		os.Exit(3)
	case "trap": // a job that shuts down cleanly on SIGTERM
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		markReady()
		<-sig
		os.Exit(0)
	case "ignore": // a job that hangs through SIGTERM
		signal.Ignore(syscall.SIGTERM)
		markReady()
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(99)
}

func markReady() {
	if err := os.WriteFile(os.Getenv(helperReady), nil, 0o600); err != nil {
		os.Exit(98)
	}
}

// runHelper starts subprocess(grace) with the given child mode. If cancel is
// set, it cancels once the child is ready. It returns the exec's result and
// how long it took after the cancel.
func runHelper(t *testing.T, mode string, grace time.Duration, cancel bool) (int, time.Duration, error) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(helperEnv, mode)
	t.Setenv(helperReady, ready)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := subprocess(grace)(ctx, []string{"-agent=support"})
		done <- result{code, err}
	}()
	var cancelled time.Time
	if cancel {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child never became ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancelled = time.Now()
		stop()
	}
	select {
	case r := <-done:
		return r.code, time.Since(cancelled), r.err
	case <-time.After(20 * time.Second):
		t.Fatal("subprocess did not return")
	}
	return 0, 0, nil
}

func TestSubprocess_NonZeroExitIsACodeNotAnError(t *testing.T) {
	if code, _, err := runHelper(t, "exit3", time.Second, false); code != 3 || err != nil {
		t.Fatalf("got (%d, %v), want (3, nil)", code, err)
	}
}

// Regression for e4c95cd: after SIGTERM, Run returns ctx's error even when
// the child exited 0; the exit status must win.
func TestSubprocess_CleanExitAfterSIGTERM(t *testing.T) {
	if code, _, err := runHelper(t, "trap", 5*time.Second, true); code != 0 || err != nil {
		t.Fatalf("got (%d, %v), want (0, nil)", code, err)
	}
}

func TestSubprocess_KilledAfterGrace(t *testing.T) {
	const grace = 100 * time.Millisecond
	code, took, err := runHelper(t, "ignore", grace, true)
	if code != -1 || err == nil {
		t.Fatalf("got (%d, %v), want (-1, error)", code, err)
	}
	if took > grace+2*time.Second {
		t.Errorf("took %v after cancel, want about the %v grace", took, grace)
	}
}
