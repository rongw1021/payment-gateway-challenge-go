package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimiterAdmitsExactly100ConcurrentRequestsAcrossClientsAndRoutes(t *testing.T) {
	limiter, err := NewRateLimiter(100)
	require.NoError(t, err)
	now := time.Now()
	limiter.now = func() time.Time { return now }
	var called, rejected atomic.Int32
	handler := limiter.Limit(slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	start := make(chan struct{})
	var work sync.WaitGroup
	for i := 0; i < 200; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			<-start
			path := []string{"/api/CardTokens", "/api/PaymentIntents", "/swagger/index.html", "/missing"}[i%4]
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.RemoteAddr = []string{"192.0.2.1:1234", "192.0.2.2:1234"}[i%2]
			request.Header.Set("Authorization", []string{"Bearer sk_first", "Bearer sk_second"}[i%2])
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code == http.StatusTooManyRequests {
				rejected.Add(1)
			} else {
				assert.Equal(t, http.StatusNoContent, response.Code)
			}
		}(i)
	}
	close(start)
	work.Wait()
	assert.EqualValues(t, 100, called.Load())
	assert.EqualValues(t, 100, rejected.Load())
}

func TestRateLimiterUsesRollingWindowAndExactExpiry(t *testing.T) {
	limiter, err := NewRateLimiter(2)
	require.NoError(t, err)
	start := time.Unix(0, 900*int64(time.Millisecond))
	now := start
	limiter.now = func() time.Time { return now }
	for _, step := range []struct {
		elapsed time.Duration
		allowed bool
	}{
		{0, true},
		{500 * time.Millisecond, true},
		{600 * time.Millisecond, false},
		{time.Second - time.Nanosecond, false},
		{time.Second, true},
		{time.Second + time.Nanosecond, false},
		{1500 * time.Millisecond, true},
		{1500 * time.Millisecond, false},
		{5 * time.Second, true},
		{5 * time.Second, true},
		{5 * time.Second, false},
	} {
		now = start.Add(step.elapsed)
		assert.Equal(t, step.allowed, limiter.allow(), "at %s", step.elapsed)
	}
}

func TestRateLimiterExemptsOnlyHealthCheckAndPreservesRejectionMetadata(t *testing.T) {
	limiter, err := NewRateLimiter(1)
	require.NoError(t, err)
	now := time.Now()
	limiter.now = func() time.Time { return now }
	handler := limiter.Limit(slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for i := 0; i < 3; i++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ping", nil))
		assert.Equal(t, http.StatusNoContent, response.Code)
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil))
	require.Equal(t, http.StatusNoContent, first.Code, "health probes must not consume quota")
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/api/PaymentIntents"}, {http.MethodGet, "/api/PaymentIntents/missing"},
		{http.MethodGet, "/swagger/index.html"}, {http.MethodGet, "/missing"},
		{http.MethodPost, "/ping"}, {http.MethodGet, "/ping/"}, {http.MethodHead, "/ping"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusTooManyRequests, response.Code)
		assert.Equal(t, "1", response.Header().Get("Retry-After"))
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
		var problem models.ProblemDetails
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		assert.Equal(t, http.StatusTooManyRequests, problem.Status)
		assert.Equal(t, "Too Many Requests", problem.Title)
		assert.NotEmpty(t, problem.RequestID)
		assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
	}
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-Id", "existing-request")
	request := httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil)
	request = request.WithContext(WithRequestID(context.Background(), "existing-request"))
	handler.ServeHTTP(response, request)
	var problem models.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, "existing-request", problem.RequestID)
	assert.Equal(t, "existing-request", response.Header().Get("X-Request-Id"))
	healthy := httptest.NewRecorder()
	handler.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assert.Equal(t, http.StatusNoContent, healthy.Code, "saturation must not block health checks")
}

func TestRateLimiterRejectsNonpositiveLimits(t *testing.T) {
	for _, limit := range []int{0, -1} {
		_, err := NewRateLimiter(limit)
		require.ErrorContains(t, err, "rate_limit.global_requests_per_second")
	}
}
