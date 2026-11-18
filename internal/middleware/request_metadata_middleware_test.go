package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			handler := RequestID(RequireIdempotencyKey(next))
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
