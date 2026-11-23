package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireIdempotencyKeyRejectsInvalidHeadersBeforeHandler(t *testing.T) {
	const validKey = "01234567-89ab-4cde-8123-456789abcdef"
	for _, test := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"missing", nil, false},
		{"empty", []string{""}, false},
		{"blank", []string{" \t"}, false},
		{"combined", []string{validKey + "," + validKey}, false},
		{"multiple", []string{validKey, "ffffffff-ffff-ffff-ffff-ffffffffffff"}, false},
		{"repeated", []string{validKey, validKey}, false},
		{"legacy key", []string{"order-123"}, false},
		{"short", []string{validKey[:35]}, false},
		{"long", []string{validKey + "0"}, false},
		{"uppercase", []string{strings.ToUpper(validKey)}, false},
		{"no hyphens", []string{strings.ReplaceAll(validKey, "-", "")}, false},
		{"non-hex", []string{validKey[:35] + "g"}, false},
		{"unicode", []string{validKey[:34] + "\u00e9"}, false},
		{"embedded space", []string{validKey[:15] + " " + validKey[16:]}, false},
		{"surrounding spaces", []string{" " + validKey + " "}, false},
		{"trailing newline", []string{validKey + "\n"}, false},
		{"valid", []string{validKey}, true},
		{"hex upper bound", []string{"ffffffff-ffff-ffff-ffff-ffffffffffff"}, true},
		{"hex lower bound", []string{"00000000-0000-0000-0000-000000000000"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				require.Len(t, test.values, 1)
				assert.Equal(t, test.values[0], IdempotencyKeyFromContext(r.Context()))
				w.WriteHeader(http.StatusNoContent)
			})
			handler := RequestID(slog.Default())(RequireIdempotencyKey(slog.Default())(next))
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			request = request.WithContext(context.WithValue(request.Context(), idempotencyKey{}, "stale-context-key"))
			for _, value := range test.values {
				request.Header.Add("X-Idempotency-Key", value)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, test.valid, called)
			if test.valid {
				assert.Equal(t, http.StatusNoContent, response.Code)
				return
			}
			require.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, http.StatusBadRequest, problem.Status)
			assert.Equal(t, []string{"Supply exactly one idempotency key in lowercase hyphenated GUID format."}, problem.Errors["idempotency_key"])
			assert.NotEmpty(t, problem.RequestID)
			assert.Equal(t, response.Header().Get("X-Request-Id"), problem.RequestID)
		})
	}
}

func TestIdempotencyKeyFromContextRequiresMiddleware(t *testing.T) {
	assert.PanicsWithValue(t, "idempotency middleware did not provide a valid key", func() {
		IdempotencyKeyFromContext(context.Background())
	})
}

type failedMiddlewareWriter struct {
	header   http.Header
	statuses []int
	writes   int
}

func (w *failedMiddlewareWriter) Header() http.Header    { return w.header }
func (w *failedMiddlewareWriter) WriteHeader(status int) { w.statuses = append(w.statuses, status) }
func (w *failedMiddlewareWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestMiddlewareLogsFailedResponsesWithInjectedLogger(t *testing.T) {
	for _, kind := range []string{"unauthorized", "forbidden", "idempotency", "rate limit", "merchant rate limit", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			logger := logging.NewConsole(&output)
			auth, err := NewAuthenticator([]APIKeyCredential{testCredential()})
			require.NoError(t, err)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("rejected request reached endpoint") })
			var handler http.Handler
			status, count := http.StatusBadRequest, 1
			switch kind {
			case "unauthorized":
				handler = auth.RequireScope(CardTokensCreate, logger)(next)
				status = http.StatusUnauthorized
			case "forbidden":
				handler = auth.RequireScope(PaymentIntentsRead, logger)(next)
				status = http.StatusForbidden
			case "idempotency":
				handler = RequireIdempotencyKey(logger)(next)
			case "rate limit":
				limiter, err := NewRateLimiter(1)
				require.NoError(t, err)
				require.True(t, limiter.allow())
				handler = limiter.Limit(logger)(next)
				status = http.StatusTooManyRequests
			case "merchant rate limit":
				id := testCredential().MerchantID
				limiter, err := NewMerchantRateLimiter([]string{id}, 1)
				require.NoError(t, err)
				require.True(t, limiter.limiters[id].allow())
				handler = auth.RequireScope(CardTokensCreate, logger)(limiter.Limit(logger)(next))
				status = http.StatusTooManyRequests
			case "recovery":
				handler = Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private-panic") }))
				status, count = http.StatusInternalServerError, 2
			}
			request := httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil)
			if kind == "forbidden" || kind == "merchant rate limit" {
				request.Header.Set("Authorization", "Bearer sk_test-payload")
			}
			response := &failedMiddlewareWriter{header: make(http.Header)}
			RequestID(logger)(handler).ServeHTTP(response, request)
			assert.Equal(t, []int{status}, response.statuses)
			assert.Equal(t, 1, response.writes)
			decoder := json.NewDecoder(&output)
			var record map[string]interface{}
			for i := 0; i < count; i++ {
				require.NoError(t, decoder.Decode(&record))
				assert.Equal(t, "ERROR", record["level"])
				assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
			}
			assert.Equal(t, "failed to write HTTP response", record["message"])
			assert.Contains(t, record["error"], io.ErrClosedPipe.Error())
			if kind == "forbidden" || kind == "merchant rate limit" {
				assert.Equal(t, testCredential().MerchantID, record["merchant_id"])
			}
			assert.ErrorIs(t, decoder.Decode(&record), io.EOF)
		})
	}
}
