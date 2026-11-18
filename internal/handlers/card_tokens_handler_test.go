package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const cardTokenBody = `{"card_number":"0222405343240007","expiry_month":4,"expiry_year":2035,"cvv":"012"}`

func cardTokenHandler(t *testing.T) (*CardTokensHandler, *services.CardTokenService, http.Handler) {
	t.Helper()
	now := func() time.Time { return time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC) }
	repo := repository.NewCardsRepository(now)
	service := services.NewCardTokenService(repo, now)
	var credentials []middleware.APIKeyCredential
	for payload, scopes := range map[string][]string{
		"cards": {middleware.CardTokensCreate}, "read": {"payment-intents:read"},
		"create": {"payment-intents:create"}, "confirm": {"payment-intents:confirm"},
	} {
		hash := sha256.Sum256([]byte(payload))
		credentials = append(credentials, middleware.APIKeyCredential{
			MerchantID: "merchant", KeyHash: hex.EncodeToString(hash[:]), Scopes: scopes,
		})
	}
	auth, err := middleware.NewAuthenticator(credentials)
	require.NoError(t, err)
	handler := NewCardTokensHandler(service)
	endpoint := middleware.NoStore(middleware.RequestID(
		auth.RequireScope(middleware.CardTokensCreate)(handler.PostHandler()),
	))
	return handler, service, endpoint
}

func TestCardTokenHandlerResponseContract(t *testing.T) {
	_, service, endpoint := cardTokenHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/CardTokens", strings.NewReader(cardTokenBody))
	req.Header.Set("Authorization", "Bearer sk_cards")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-Request-Id", "untrusted-client-id")
	w := httptest.NewRecorder()
	endpoint.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", w.Header().Get("Pragma"))
	var response models.CardTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", response.Token)
	assert.NotEqual(t, response.Token, response.RequestID)
	assert.Equal(t, time.Date(2030, 4, 10, 12, 15, 0, 0, time.UTC), response.ExpiresAt)
	assert.Equal(t, w.Header().Get("X-Request-Id"), response.RequestID)
	assert.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", response.RequestID)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))
	assert.Len(t, fields, 3)
	for _, field := range []string{"token", "expires_at", "request_id"} {
		assert.Contains(t, fields, field)
	}
	card, err := service.TryConsume(context.Background(), "merchant", response.Token)
	require.NoError(t, err)
	require.NotNil(t, card)
	assert.Equal(t, "0222405343240007", card.CardNumber)
	assert.Equal(t, "012", card.Cvv)
}

func TestCardTokenHandlerRejectsRequests(t *testing.T) {
	_, _, endpoint := cardTokenHandler(t)
	for _, test := range []struct {
		name, authorization, contentType, body string
		status                                 int
	}{
		{"no auth", "", "application/json", cardTokenBody, 401},
		{"unknown auth", "Bearer sk_unknown", "application/json", cardTokenBody, 401},
		{"read scope", "Bearer sk_read", "application/json", cardTokenBody, 403},
		{"create scope", "Bearer sk_create", "application/json", cardTokenBody, 403},
		{"confirm scope", "Bearer sk_confirm", "application/json", cardTokenBody, 403},
		{"missing type", "Bearer sk_cards", "", cardTokenBody, 415},
		{"wrong type", "Bearer sk_cards", "text/plain", cardTokenBody, 415},
		{"empty body", "Bearer sk_cards", "application/json", "", 400},
		{"null body", "Bearer sk_cards", "application/json", "null", 400},
		{"array body", "Bearer sk_cards", "application/json", "[]", 400},
		{"malformed body", "Bearer sk_cards", "application/json", "{", 400},
		{"missing fields", "Bearer sk_cards", "application/json", "{}", 400},
		{"short card number", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"0222405343240007"`, `"123"`, 1), 400},
		{"numeric number", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"0222405343240007"`, "2222405343240007", 1), 400},
		{"numeric cvv", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"012"`, "123", 1), 400},
		{"fractional month", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"expiry_month":4`, `"expiry_month":4.5`, 1), 400},
		{"null field", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"expiry_year":2035`, `"expiry_year":null`, 1), 400},
		{"invalid cvv", "Bearer sk_cards", "application/json", strings.Replace(cardTokenBody, `"012"`, `"12x"`, 1), 400},
		{"trailing object", "Bearer sk_cards", "application/json", cardTokenBody + "{}", 400},
		{"trailing null", "Bearer sk_cards", "application/json", cardTokenBody + "null", 400},
		{"trailing junk", "Bearer sk_cards", "application/json", cardTokenBody + "x", 400},
		{"too large", "Bearer sk_cards", "application/json", cardTokenBody + strings.Repeat(" ", 1<<20), 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/CardTokens", strings.NewReader(test.body))
			req.Header.Set("Authorization", test.authorization)
			req.Header.Set("Content-Type", test.contentType)
			w := httptest.NewRecorder()
			endpoint.ServeHTTP(w, req)
			require.Equal(t, test.status, w.Code)
			assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
			assert.Equal(t, test.status, problem.Status)
			assert.NotEmpty(t, problem.RequestID)
			assert.Equal(t, w.Header().Get("X-Request-Id"), problem.RequestID)
			assert.NotContains(t, w.Body.String(), "0222405343240007")
			if test.status == 401 {
				assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
			}
			if test.name == "missing fields" {
				for _, field := range []string{"card_number", "expiry_month", "expiry_year", "cvv"} {
					assert.Contains(t, problem.Errors, field)
				}
			}
			if test.name == "short card number" {
				assert.NotEmpty(t, problem.Errors["card_number"])
			}
		})
	}
}

func TestCardTokenHandlerTracesOnlyHandlerExecutionWithoutCardData(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTextMapPropagator(previous)
	for _, test := range []struct {
		name, authorization, body string
		status                    int
		fail                      bool
	}{
		{"issued", "Bearer sk_cards", cardTokenBody, 200, false},
		{"binding error", "Bearer sk_cards", "{", 400, false},
		{"issuance error", "Bearer sk_cards", cardTokenBody, 500, true},
		{"before handler", "", cardTokenBody, 401, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
			handler, _, endpoint := cardTokenHandler(t)
			handler.tracer = provider.Tracer("test")
			if test.fail {
				handler.service = failingCardTokenIssuer{}
			}
			request := httptest.NewRequest(http.MethodPost, "/api/CardTokens?cvv=private-query", strings.NewReader(test.body))
			request.Header.Set("Authorization", test.authorization)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
			response := httptest.NewRecorder()
			endpoint.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code)
			spans := recorder.Ended()
			if test.status == 401 {
				assert.Empty(t, spans)
				return
			}
			require.Len(t, spans, 1)
			span := spans[0]
			assert.Equal(t, "POST /api/CardTokens", span.Name())
			assert.Equal(t, trace.SpanKindServer, span.SpanKind())
			assert.Equal(t, "0123456789abcdef0123456789abcdef", span.SpanContext().TraceID().String())
			assert.Equal(t, "0123456789abcdef", span.Parent().SpanID().String())
			assert.True(t, span.Parent().IsRemote())
			assert.False(t, span.EndTime().Before(span.StartTime()))
			attributes := make(map[string]interface{})
			for _, attr := range span.Attributes() {
				attributes[string(attr.Key)] = attr.Value.AsInterface()
			}
			assert.EqualValues(t, test.status, attributes["http.response.status_code"])
			assert.Equal(t, response.Header().Get("X-Request-Id"), attributes["request_id"])
			assert.Equal(t, "/api/CardTokens", attributes["http.route"])
			if test.fail {
				assert.Equal(t, codes.Error, span.Status().Code)
			} else {
				assert.Equal(t, codes.Unset, span.Status().Code)
			}
			for _, secret := range []string{"0222405343240007", "cvv", "sk_cards", "private-query", "storage failure"} {
				assert.NotContains(t, fmt.Sprint(attributes, span.Events(), span.Status()), secret)
			}
		})
	}
}

type failingCardTokenIssuer struct{}

func (failingCardTokenIssuer) Issue(context.Context, string, models.CreateCardTokenRequest) (*models.CardTokenResponse, error) {
	return nil, errors.New("internal storage failure")
}

func TestCardTokenHandlerHidesInternalFailure(t *testing.T) {
	handler, _, endpoint := cardTokenHandler(t)
	handler.service = failingCardTokenIssuer{}
	req := httptest.NewRequest(http.MethodPost, "/api/CardTokens", strings.NewReader(cardTokenBody))
	req.Header.Set("Authorization", "Bearer sk_cards")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	endpoint.ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
	assert.NotContains(t, w.Body.String(), "storage")
	assert.NotContains(t, w.Body.String(), "token")
}
