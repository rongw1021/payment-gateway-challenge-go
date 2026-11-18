package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
)

type merchantKey struct{}

// MerchantIDFromContext requires RequireScope; a missing identity is a wiring error.
func MerchantIDFromContext(ctx context.Context) string {
	merchantID, ok := ctx.Value(merchantKey{}).(string)
	if !ok || strings.TrimSpace(merchantID) == "" {
		panic("authentication middleware did not provide a merchant ID")
	}
	return merchantID
}

func (a *Authenticator) RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := GetRequestID(r.Context())
			merchant, err := a.Authenticate(r.Header)
			if err != nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpresponse.WriteProblem(w, http.StatusUnauthorized, requestID, nil)
				return
			}
			if strings.TrimSpace(merchant.ID) == "" || !merchant.Allows(scope) {
				httpresponse.WriteProblem(w, http.StatusForbidden, requestID, nil)
				return
			}
			ctx := context.WithValue(r.Context(), merchantKey{}, merchant.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
