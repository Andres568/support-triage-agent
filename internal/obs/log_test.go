package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSeverity(t *testing.T) {
	for l, want := range map[slog.Level]string{
		slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING",
		slog.LevelError: "ERROR", slog.LevelError + 4: "ERROR",
	} {
		if got := severity(l); got != want {
			t.Errorf("severity(%v) = %s, want %s", l, got, want)
		}
	}
}

func logLine(t *testing.T, write func(*slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	write(NewLogger(&buf, "proj").With("agent", "support"))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not one JSON line: %q: %v", buf.String(), err)
	}
	return m
}

func TestNewLogger_CloudLoggingFields(t *testing.T) {
	m := logLine(t, func(l *slog.Logger) { l.Warn("lease lost", "id", 7) })
	if m["severity"] != "WARNING" || m["message"] != "lease lost" || m["agent"] != "support" || m["id"] != 7.0 {
		t.Errorf("line = %v", m)
	}
	for _, k := range []string{"level", "msg", logTraceKey, logSpanKey, logSampledKey} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected key %q without a span: %v", k, m)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, m["time"].(string)); err != nil {
		t.Errorf("time: %v", err)
	}
}

func TestNewLogger_TraceFieldsFromSpan(t *testing.T) {
	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "s")
	defer span.End()
	sc := span.SpanContext()

	m := logLine(t, func(l *slog.Logger) { l.InfoContext(ctx, "item finalized") })
	if m[logTraceKey] != "projects/proj/traces/"+sc.TraceID().String() ||
		m[logSpanKey] != sc.SpanID().String() || m[logSampledKey] != true {
		t.Errorf("trace fields = %v", m)
	}
}
