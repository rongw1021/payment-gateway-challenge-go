package scenarios

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptIncludesFullDevelopmentRequestAndResponse(t *testing.T) {
	input := request{method: http.MethodPost, path: "/api/CardTokens", description: "Tokenize a development card"}
	requestBody := []byte(`{"card_number":"2222405343240007","cvv":"012"}`)
	req := httptest.NewRequest(input.method, "http://localhost:8090"+input.path, strings.NewReader(string(requestBody)))
	req.Header.Set("Authorization", "Bearer sk_development")
	req.Header.Set("Content-Type", "application/json")
	req.Header["X-Idempotency-Key"] = []string{"first", "second"}
	responseBody := []byte(`{"token":"development-token","request_id":"request-123"}`)
	reply := response{status: http.StatusOK, headers: http.Header{
		"Content-Type": {"application/json"}, "X-Request-Id": {"request-123"},
	}}
	transcript := formatExchange("TestIntegrationScenarios/Token", input, req, requestBody, reply, responseBody, nil)
	for _, expected := range []string{
		"Test case: TestIntegrationScenarios/Token", "Description: Tokenize a development card",
		"Request: POST http://localhost:8090/api/CardTokens", "Request headers (explicit):",
		"Authorization: Bearer sk_development", "X-Idempotency-Key: first", "X-Idempotency-Key: second",
		`"card_number": "2222405343240007"`, `"cvv": "012"`,
		"Response status: 200 OK", "Response headers:", "X-Request-Id: request-123",
		"Response body", `"token": "development-token"`,
	} {
		assert.Contains(t, transcript, expected)
	}
	assert.Equal(t, 2, strings.Count(transcript, divider))
	assert.NotContains(t, transcript, "[REDACTED]")
	assert.Equal(t, []byte(`{"token":"development-token","request_id":"request-123"}`), responseBody)
}

func TestTranscriptPreservesInvalidBodiesAndUnavailableResponse(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/CardTokens", nil)
	transcript := formatExchange("InvalidJSON", request{method: http.MethodPost, path: "/api/CardTokens"},
		req, []byte("{"), response{status: 502, headers: http.Header{"Content-Type": {"text/plain"}}},
		[]byte("upstream unavailable"), errors.New("gateway returned non-object JSON"))
	assert.Contains(t, transcript, "(non-JSON body)\n{")
	assert.Contains(t, transcript, "Response status: 502 Bad Gateway")
	assert.Contains(t, transcript, "upstream unavailable")
	assert.Contains(t, transcript, "Error: gateway returned non-object JSON")

	transcript = formatExchange("Canceled", request{method: http.MethodGet, path: "/api/PaymentIntents/id"},
		req, nil, response{}, nil, context.Canceled)
	assert.Contains(t, transcript, "Response status: (not received)")
	assert.Contains(t, transcript, "Request body (0 bytes):\n  (none)")
	assert.Contains(t, transcript, "Error: context canceled")
}

func TestTranscriptReportsWireSizeWithoutDumpingInsignificantJSONWhitespace(t *testing.T) {
	body := []byte("{}" + strings.Repeat(" ", 1<<20))
	transcript := formatExchange("OversizedJSON", request{method: http.MethodPost, path: "/api/CardTokens"},
		nil, body, response{}, nil, nil)
	assert.Contains(t, transcript, "Request body (1048578 bytes)")
	assert.Less(t, len(transcript), 1000)
}

func TestClientRetainsResponseMetadataWhenJSONCannotBeParsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Request-Id", "failed-request")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream unavailable"))
	}))
	defer server.Close()
	c := &client{http: server.Client(), baseURL: server.URL}
	reply, err := c.do(t, request{method: http.MethodGet, path: "/api/PaymentIntents/id"})
	require.ErrorContains(t, err, "non-object JSON")
	assert.Equal(t, http.StatusBadGateway, reply.status)
	assert.Equal(t, "failed-request", reply.headers.Get("X-Request-Id"))
}
