package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestLoggingRecordsSafeRouteMetadataAtInfoLevel(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 409, 500, 503} {
		var output bytes.Buffer
		router := chi.NewRouter()
		router.Use(RequestLogging(logging.NewConsole(&output)))
		router.Post("/api/PaymentIntents/{paymentIntentId}/confirm", func(w http.ResponseWriter, r *http.Request) {
			assert.NotEmpty(t, GetRequestID(r.Context()))
			time.Sleep(time.Millisecond)
			w.WriteHeader(status)
		})
		request := httptest.NewRequest(http.MethodPost, "/api/PaymentIntents/private-token/confirm?card_number=0222405343240007",
			strings.NewReader(`{"cvv":"012","card_token":"private-token"}`))
		request.Header.Set("Authorization", "Bearer private-api-key")
		request.Header.Set("X-Request-Id", "untrusted-id")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, status, response.Code)
		assert.Equal(t, 1, strings.Count(output.String(), "\n"))
		var record map[string]interface{}
		require.NoError(t, json.Unmarshal(output.Bytes(), &record))
		assert.Equal(t, "INFO", record["level"])
		assert.NotContains(t, record, "action")
		assert.Equal(t, http.MethodPost, record["method"])
		assert.Equal(t, "/api/PaymentIntents/{paymentIntentId}/confirm", record["route"])
		assert.EqualValues(t, status, record["status_code"])
		assert.GreaterOrEqual(t, record["latency_ms"], float64(1))
		assert.Equal(t, response.Header().Get("X-Request-Id"), record["request_id"])
		for _, secret := range []string{"private-token", "0222405343240007", "private-api-key", "cvv", "untrusted-id"} {
			assert.NotContains(t, output.String(), secret)
		}
	}
}

func TestRequestLoggingCoversUnknownPaymentRoutesAndExcludesOtherEndpoints(t *testing.T) {
	for _, test := range []struct {
		path   string
		logged bool
	}{
		{"/api/CardTokens", true},
		{"/api/PaymentIntents/private-extra/path", true},
		{"/ping", false},
		{"/swagger/index.html", false},
		{"/api/PaymentIntents-other", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			var output bytes.Buffer
			router := chi.NewRouter()
			router.Use(RequestLogging(logging.NewConsole(&output)))
			router.Post("/api/CardTokens", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if !test.logged {
				assert.Empty(t, output.String())
				return
			}
			var record map[string]interface{}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			assert.EqualValues(t, response.Code, record["status_code"])
			assert.Equal(t, http.MethodGet, record["method"])
			assert.NotEmpty(t, record["route"])
			assert.NotEmpty(t, record["request_id"])
			assert.NotContains(t, output.String(), "private-extra")
		})
	}
}

func TestRequestLoggingRecordsPanicWithoutSuppressingIt(t *testing.T) {
	var output bytes.Buffer
	router := chi.NewRouter()
	router.Use(RequestLogging(logging.NewConsole(&output)))
	router.Post("/api/PaymentIntents", func(http.ResponseWriter, *http.Request) { panic("test panic") })
	assert.PanicsWithValue(t, "test panic", func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/PaymentIntents", nil))
	})
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	assert.EqualValues(t, 500, record["status_code"])
	assert.Equal(t, "INFO", record["level"])
}

func TestRequestLoggingRecordsMethodAndRouteWithImplicitSuccessfulResponses(t *testing.T) {
	for _, test := range []struct {
		method, route, path string
	}{
		{http.MethodPost, "/api/CardTokens", "/api/CardTokens"},
		{http.MethodPost, "/api/PaymentIntents", "/api/PaymentIntents"},
		{http.MethodGet, "/api/PaymentIntents/{paymentIntentId}", "/api/PaymentIntents/private-id"},
	} {
		t.Run(test.method+" "+test.route, func(t *testing.T) {
			var output bytes.Buffer
			router := chi.NewRouter()
			router.Use(RequestLogging(logging.NewConsole(&output)))
			router.MethodFunc(test.method, test.route, func(http.ResponseWriter, *http.Request) {})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(test.method, test.path, nil))
			var record map[string]interface{}
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			assert.Equal(t, test.method, record["method"])
			assert.Equal(t, test.route, record["route"])
			assert.EqualValues(t, http.StatusOK, record["status_code"])
			assert.NotEmpty(t, record["request_id"])
		})
	}
}
