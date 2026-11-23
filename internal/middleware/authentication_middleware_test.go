package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationMiddlewareRequiresMerchantAndRouteScope(t *testing.T) {
	for _, test := range []struct {
		name, authorization, merchantID, scope string
		status                                 int
	}{
		{"missing key", "", "merchant", CardTokensCreate, http.StatusUnauthorized},
		{"unknown key", "Bearer sk_unknown", "merchant", CardTokensCreate, http.StatusUnauthorized},
		{"missing scope", "Bearer sk_test-payload", "merchant", PaymentIntentsRead, http.StatusForbidden},
		{"missing merchant", "Bearer sk_test-payload", "", CardTokensCreate, http.StatusForbidden},
		{"blank merchant", "Bearer sk_test-payload", " \t", CardTokensCreate, http.StatusForbidden},
		{"allowed", "Bearer sk_test-payload", "merchant", CardTokensCreate, http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth, err := NewAuthenticator([]APIKeyCredential{testCredential()})
			require.NoError(t, err)
			// Exercise the runtime identity guard independently of configuration validation.
			auth.credentials[0].merchant.ID = test.merchantID
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				assert.Equal(t, "merchant", MerchantIDFromContext(r.Context()))
				assert.NotEmpty(t, GetRequestID(r.Context()))
				w.WriteHeader(http.StatusNoContent)
			})
			handler := NoStore(RequestID(slog.Default())(auth.RequireScope(test.scope, slog.Default())(next)))
			req := httptest.NewRequest(http.MethodPost, "/protected", nil)
			req = req.WithContext(context.WithValue(req.Context(), merchantKey{}, "untrusted-merchant"))
			req.Header.Set("Authorization", test.authorization)
			req.Header.Set("X-Merchant-Id", "untrusted-merchant")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			require.Equal(t, test.status, w.Code)
			assert.Equal(t, test.status == http.StatusNoContent, called)
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			assert.Equal(t, "no-cache", w.Header().Get("Pragma"))
			if test.status != http.StatusNoContent {
				assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
				var problem models.ProblemDetails
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
				assert.Equal(t, test.status, problem.Status)
				assert.NotEmpty(t, problem.RequestID)
				assert.Equal(t, w.Header().Get("X-Request-Id"), problem.RequestID)
			}
			if test.status == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
			} else {
				assert.Empty(t, w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestMerchantIDFromContextRejectsMissingMiddleware(t *testing.T) {
	assert.PanicsWithValue(t, "authentication middleware did not provide a merchant ID", func() {
		MerchantIDFromContext(context.Background())
	})
}
