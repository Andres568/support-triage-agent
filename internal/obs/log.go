package obs

import (
	"context"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Cloud Logging's special JSON fields; see
// https://cloud.google.com/logging/docs/structured-logging.
const (
	logTraceKey   = "logging.googleapis.com/trace"
	logSpanKey    = "logging.googleapis.com/spanId"
	logSampledKey = "logging.googleapis.com/trace_sampled"
)

// NewLogger writes JSON that Cloud Logging parses: severity, message, an
// RFC 3339 time, and the trace of the span in the record's context, so a log
// line links to its trace. Use the *Context methods to get the trace fields.
// project is the GCP project id; empty writes the bare trace id.
func NewLogger(w io.Writer, project string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: cloudAttr})
	return slog.New(traceHandler{Handler: h, project: project})
}

func cloudAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		return slog.String("severity", severity(a.Value.Any().(slog.Level)))
	case slog.MessageKey:
		a.Key = "message"
	case slog.TimeKey:
		return slog.String("time", a.Value.Time().Format(time.RFC3339Nano))
	}
	return a
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

type traceHandler struct {
	slog.Handler
	project string
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		id := sc.TraceID().String()
		if h.project != "" {
			id = "projects/" + h.project + "/traces/" + id
		}
		r.AddAttrs(slog.String(logTraceKey, id), slog.String(logSpanKey, sc.SpanID().String()),
			slog.Bool(logSampledKey, sc.IsSampled()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(as), project: h.project}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name), project: h.project}
}
