package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/requestcontext"
)

type merchantKey = requestcontext.MerchantIDKey

// MerchantIDFromContext requires RequireScope; a missing identity is a wiring error.
func MerchantIDFromContext(ctx context.Context) string {
	merchantID, ok := ctx.Value(merchantKey{}).(string)
	if !ok || strings.TrimSpace(merchantID) == "" {
		panic("authentication middleware did not provide a merchant ID")
	}
	return merchantID
}

func (a *Authenticator) RequireScope(scope string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := GetRequestID(r.Context())
			merchant, err := a.Authenticate(r.Header)
			if err != nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				if err := httpresponse.WriteProblem(w, http.StatusUnauthorized, requestID, nil); err != nil {
					logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
				}
				return
			}
			ctx := context.WithValue(r.Context(), merchantKey{}, merchant.ID)
			if strings.TrimSpace(merchant.ID) == "" || !merchant.Allows(scope) {
				if err := httpresponse.WriteProblem(w, http.StatusForbidden, requestID, nil); err != nil {
					logger.ErrorContext(ctx, "failed to write HTTP response", "error", err)
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
