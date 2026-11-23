package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
)

type RateLimiter struct {
	mu       sync.Mutex
	limit    int
	accepted []time.Time
	oldest   int
	now      func() time.Time
}

func NewRateLimiter(requestsPerSecond int) (*RateLimiter, error) {
	if requestsPerSecond <= 0 {
		return nil, errors.New("rate_limit.global_requests_per_second must be positive")
	}
	return &RateLimiter{limit: requestsPerSecond, now: time.Now}, nil
}

func (l *RateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.accepted) < l.limit {
		l.accepted = append(l.accepted, now)
		return true
	}
	// Retain the last N admissions in a ring; the oldest must leave the window.
	if now.Sub(l.accepted[l.oldest]) < time.Second {
		return false
	}
	l.accepted[l.oldest] = now
	l.oldest = (l.oldest + 1) % l.limit
	return true
}

func (l *RateLimiter) Limit(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		reject := NoStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			if err := httpresponse.WriteProblem(w, http.StatusTooManyRequests, GetRequestID(r.Context()), nil); err != nil {
				logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
			}
		}))
		rejectWithMetadata := RequestID(logger)(reject)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/ping" {
				next.ServeHTTP(w, r)
				return
			}
			if l.allow() {
				next.ServeHTTP(w, r)
				return
			}
			// Non-payment routes do not otherwise run request-ID middleware.
			if GetRequestID(r.Context()) == "" {
				rejectWithMetadata.ServeHTTP(w, r)
				return
			}
			reject.ServeHTTP(w, r)
		})
	}
}
