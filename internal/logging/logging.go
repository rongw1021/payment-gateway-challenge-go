// Package logging creates injectable slog loggers with console and optional OTLP output.
// Context-aware calls include available request, merchant, payment-intent and bank-transaction
// IDs; explicit attributes take precedence. Context fields follow the logger's current group.
package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

type combinedHandler struct {
	console slog.Handler
	export  slog.Handler
}

func jsonHandler(output io.Writer) slog.Handler {
	return slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 {
				switch attr.Key {
				case slog.TimeKey:
					return slog.Time("@timestamp", attr.Value.Time().UTC())
				case slog.MessageKey:
					attr.Key = "message"
				}
			}
			return attr
		},
	})
}

func NewConsole(output io.Writer) *slog.Logger {
	return slog.New(&contextHandler{Handler: jsonHandler(output)})
}

func New(output io.Writer) (*slog.Logger, error) {
	if output == nil {
		return nil, errors.New("console log output is required")
	}
	return slog.New(&contextHandler{Handler: &combinedHandler{
		console: jsonHandler(output),
		export:  otelslog.NewHandler("payment-gateway/logging"),
	}}), nil
}

func (h *combinedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && (h.console.Enabled(ctx, level) || h.export.Enabled(ctx, level))
}

func (h *combinedHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range []slog.Handler{h.console, h.export} {
		if handler.Enabled(ctx, record.Level) {
			if err := handler.Handle(ctx, record.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (h *combinedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &combinedHandler{console: h.console.WithAttrs(attrs), export: h.export.WithAttrs(attrs)}
}

func (h *combinedHandler) WithGroup(name string) slog.Handler {
	return &combinedHandler{console: h.console.WithGroup(name), export: h.export.WithGroup(name)}
}
