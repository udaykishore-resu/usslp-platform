package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logger is the platform's structured logger. Every line is JSON, every line
// carries the service identity, and every line emitted while handling a request
// carries the trace and tenant context — which is what makes it possible to
// pull one tenant's records out of a shared log index without a full text
// search.
type Logger struct {
	*slog.Logger
}

// LogConfig configures a logger.
type LogConfig struct {
	Service string
	Version string
	Region  string
	// Level is "debug", "info", "warn" or "error".
	Level string
	// Format is "json" (production) or "text" (developer terminals).
	Format string
	Output io.Writer
}

// NewLogger builds a logger.
func NewLogger(cfg LogConfig) *Logger {
	if cfg.Output == nil {
		cfg.Output = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	var h slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		h = slog.NewTextHandler(cfg.Output, opts)
	} else {
		h = slog.NewJSONHandler(cfg.Output, opts)
	}
	attrs := []slog.Attr{slog.String("service", cfg.Service)}
	if cfg.Version != "" {
		attrs = append(attrs, slog.String("version", cfg.Version))
	}
	if cfg.Region != "" {
		attrs = append(attrs, slog.String("region", cfg.Region))
	}
	return &Logger{slog.New(h.WithAttrs(attrs))}
}

// ParseLevel maps a configured level name to a slog level. Anything
// unrecognised — including the empty string — is Info, because a typo in
// LOG_LEVEL must not silence a service.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NopLogger discards output. Used in tests that assert on behaviour rather than
// on log lines.
func NopLogger() *Logger {
	return &Logger{slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))}
}

// With returns a logger with additional fields bound.
func (l *Logger) With(args ...any) *Logger { return &Logger{l.Logger.With(args...)} }

// WithTenant binds tenant and store, the two fields every operator filters on.
func (l *Logger) WithTenant(tenant, store string) *Logger {
	if store == "" {
		return l.With("tenant_id", tenant)
	}
	return l.With("tenant_id", tenant, "store_id", store)
}

// FromContext returns a logger enriched with the trace context in ctx, so a log
// line written deep inside a handler is joinable to the trace that produced it
// without the caller threading fields through every function signature.
func (l *Logger) FromContext(ctx context.Context) *Logger {
	sc := SpanContextFrom(ctx)
	if !sc.Valid() {
		return l
	}
	return l.With("trace_id", sc.TraceID, "span_id", sc.SpanID)
}
