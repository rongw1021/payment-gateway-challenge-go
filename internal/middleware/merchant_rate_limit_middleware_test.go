package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMerchantLimitsShareKeysAndRoutesButNotMerchants(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var credentials []APIKeyCredential
	for _, key := range []struct {
		payload, merchant string
		scopes            []string
	}{
		{"a1", "a", []string{CardTokensCreate, PaymentIntentsCreate}},
		{"a2", "a", []string{CardTokensCreate, PaymentIntentsCreate}},
		{"read", "a", []string{PaymentIntentsRead}},
		{"b1", "b", []string{CardTokensCreate, PaymentIntentsCreate}},
	} {
		hash := sha256.Sum256([]byte(key.payload))
		credentials = append(credentials, APIKeyCredential{
			MerchantID: key.merchant, KeyHash: hex.EncodeToString(hash[:]), Scopes: key.scopes,
		})
	}
	auth, err := NewAuthenticator(credentials)
	require.NoError(t, err)
	merchant, err := NewMerchantRateLimiter([]string{"a", "a", "b"}, 10)
	require.NoError(t, err)
	require.Len(t, merchant.limiters, 2)
	global, err := NewRateLimiter(20)
	require.NoError(t, err)
	start := time.Now()
	now := start
	global.now = func() time.Time { return now }
	for _, limiter := range merchant.limiters {
		limiter.now = func() time.Time { return now }
	}
	router := chi.NewRouter()
	router.Use(RequestID(logger), global.Limit(logger))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.With(auth.RequireScope(CardTokensCreate, logger), merchant.Limit(logger)).Post("/api/CardTokens", next)
	router.With(auth.RequireScope(PaymentIntentsCreate, logger), merchant.Limit(logger)).Post("/api/PaymentIntents", next)
	router.Get("/ping", next)
	send := func(path, key string, status int) {
		t.Helper()
		method := http.MethodPost
		if path == "/ping" {
			method = http.MethodGet
		}
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("Authorization", "Bearer sk_"+key)
		request.Header.Set("X-Merchant-Id", "untrusted")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code)
		if status == http.StatusTooManyRequests {
			assert.Equal(t, "1", response.Header().Get("Retry-After"))
			assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.NotEmpty(t, response.Header().Get("X-Request-Id"))
			assert.Empty(t, response.Header().Get("WWW-Authenticate"))
		}
	}
	send("/api/CardTokens", "unknown", 401)
	send("/api/CardTokens", "read", 403)
	for i := 0; i < 10; i++ {
		send([]string{"/api/CardTokens", "/api/PaymentIntents"}[i%2], []string{"a1", "a2"}[i%2], 204)
	}
	send("/api/CardTokens", "a2", 429)
	send("/api/PaymentIntents", "a1", 429)
	send("/api/CardTokens", "b1", 204)
	// Both rejected merchant requests count globally, as do failed authentication checks.
	for i := 0; i < 5; i++ {
		send("/api/CardTokens", "unknown", 401)
	}
	send("/api/CardTokens", "b1", 429)
	send("/ping", "", 204)
	now = start.Add(time.Second - time.Nanosecond)
	send("/api/CardTokens", "a1", 429)
	now = start.Add(time.Second)
	send("/api/CardTokens", "a1", 204)
	send("/api/CardTokens", "b1", 204)
}

func TestMerchantLimitsAreConcurrentAndExpireIndependently(t *testing.T) {
	limiter, err := NewMerchantRateLimiter([]string{"a", "b"}, 10)
	require.NoError(t, err)
	now := time.Now()
	for _, quota := range limiter.limiters {
		quota.now = func() time.Time { return now }
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var admitted [2]atomic.Int32
	var work sync.WaitGroup
	start := make(chan struct{})
	handlers := []http.Handler{
		limiter.Limit(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })),
		limiter.Limit(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })),
	}
	for i := 0; i < 100; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			<-start
			merchant := []string{"a", "b"}[i%2]
			request := httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil)
			request = request.WithContext(context.WithValue(request.Context(), merchantKey{}, merchant))
			response := httptest.NewRecorder()
			handlers[(i/2)%2].ServeHTTP(response, request)
			if response.Code == 204 {
				admitted[i%2].Add(1)
			} else {
				assert.Equal(t, 429, response.Code)
			}
		}(i)
	}
	close(start)
	work.Wait()
	assert.EqualValues(t, 10, admitted[0].Load())
	assert.EqualValues(t, 10, admitted[1].Load())
	now = now.Add(time.Second)
	for i := 0; i < 10; i++ {
		assert.True(t, limiter.limiters["a"].allow())
	}
	assert.False(t, limiter.limiters["a"].allow())
	assert.True(t, limiter.limiters["b"].allow())
}

func TestMerchantLimiterRejectsInvalidConfigurationAndUnknownIdentity(t *testing.T) {
	for _, limit := range []int{0, -1} {
		_, err := NewMerchantRateLimiter([]string{"merchant"}, limit)
		require.ErrorContains(t, err, "per_merchant_requests_per_second")
	}
	for _, ids := range [][]string{nil, {""}, {" "}} {
		_, err := NewMerchantRateLimiter(ids, 10)
		require.Error(t, err)
	}
	limiter, err := NewMerchantRateLimiter([]string{"known"}, 10)
	require.NoError(t, err)
	handler := limiter.Limit(slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unknown merchant reached handler")
	}))
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), merchantKey{}, "unknown"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Len(t, limiter.limiters, 1, "unknown identities must not allocate new quotas")
}
