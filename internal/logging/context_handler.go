package logging

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/requestcontext"
)

// contextHandler enriches all log levels without changing the slog.Logger API.
// Explicit attributes take precedence; missing and incorrectly typed IDs are omitted.
type contextHandler struct {
	slog.Handler
	keys []string
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	record = record.Clone()
	for _, field := range []struct {
		name string
		key  any
	}{
		{"request_id", requestcontext.RequestIDKey{}},
		{"merchant_id", requestcontext.MerchantIDKey{}},
		{"payment_intent_id", requestcontext.PaymentIntentIDKey{}},
		{"bank_transaction_id", requestcontext.BankTransactionIDKey{}},
	} {
		value, ok := ctx.Value(field.key).(string)
		if !ok || value == "" || slices.Contains(h.keys, field.name) {
			continue
		}
		exists := false
		record.Attrs(func(attr slog.Attr) bool {
			exists = attr.Key == field.name
			return !exists
		})
		if !exists {
			record.AddAttrs(slog.String(field.name, value))
		}
	}
	return h.Handler.Handle(ctx, record)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	keys := append([]string(nil), h.keys...)
	for _, attr := range attrs {
		if !attr.Equal(slog.Attr{}) {
			keys = append(keys, attr.Key)
		}
	}
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs), keys: keys}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name), keys: h.keys}
}
