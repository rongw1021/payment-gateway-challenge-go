package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
)

type MerchantRateLimiter struct {
	limiters map[string]*RateLimiter
}

func NewMerchantRateLimiter(merchantIDs []string, requestsPerSecond int) (*MerchantRateLimiter, error) {
	if requestsPerSecond <= 0 {
		return nil, errors.New("rate_limit.per_merchant_requests_per_second must be positive")
	}
	limiters := make(map[string]*RateLimiter, len(merchantIDs))
	for _, id := range merchantIDs {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("merchant ID is required for rate limiting")
		}
		if _, exists := limiters[id]; exists {
			continue
		}
		limiter, err := NewRateLimiter(requestsPerSecond)
		if err != nil {
			return nil, err
		}
		limiters[id] = limiter
	}
	if len(limiters) == 0 {
		return nil, errors.New("at least one merchant is required for rate limiting")
	}
	return &MerchantRateLimiter{limiters: limiters}, nil
}

// Limit must run after RequireScope. Quotas are shared across routes and API keys
// for each configured merchant; the map is immutable after construction.
func (l *MerchantRateLimiter) Limit(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		handlers := make(map[string]http.Handler, len(l.limiters))
		for id, limiter := range l.limiters {
			handlers[id] = limiter.Limit(logger)(next)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			merchantID := MerchantIDFromContext(r.Context())
			handler, exists := handlers[merchantID]
			if !exists {
				logger.ErrorContext(r.Context(), "failed to apply merchant rate limit",
					"error", errors.New("merchant quota is not configured"))
				if err := httpresponse.WriteProblem(w, http.StatusInternalServerError, GetRequestID(r.Context()), nil); err != nil {
					logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
				}
				return
			}
			handler.ServeHTTP(w, r)
		})
	}
}
