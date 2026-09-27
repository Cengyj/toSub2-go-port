package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// New creates a structured logger. Sensitive fields should never be supplied
// by callers; the ReplaceAttr hook also redacts common credential names.
func New() *slog.Logger { return NewWithWriter(os.Stderr) }
func NewWithWriter(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redactAttr}))
}

func redactAttr(groups []string, a slog.Attr) slog.Attr {
	for _, group := range groups {
		if SensitiveKey(group) {
			return slog.String(a.Key, Redacted)
		}
	}
	if SensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactText(a.Value.String()))
	case slog.KindAny:
		return slog.Any(a.Key, RedactValue(a.Value.Any()))
	default:
		return a
	}
}

func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}
func FromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return fallback
}

type loggerKey struct{}
