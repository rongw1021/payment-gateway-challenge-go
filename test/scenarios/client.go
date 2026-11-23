package scenarios

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type Credentials struct {
	Primary  string
	Other    string
	ReadOnly string
}

type client struct {
	http       *http.Client
	baseURL    string
	keys       Credentials
	lastSentAt time.Time
}

type request struct {
	method, path, key, credential string
	description                   string
	body                          interface{}
	headers                       http.Header
}

type response struct {
	status  int
	headers http.Header
	body    map[string]json.RawMessage
}

type transaction struct {
	ID        string     `json:"bank_transaction_id"`
	Status    string     `json:"status"`
	LastFour  string     `json:"card_number_last_four"`
	Month     int        `json:"expiry_month"`
	Year      int        `json:"expiry_year"`
	CreatedAt time.Time  `json:"created_at"`
	Completed *time.Time `json:"completed_at"`
}

func newClient(t *testing.T, address string, keys Credentials) *client {
	t.Helper()
	base, err := url.Parse(address)
	require.NoError(t, err, "invalid gateway URL")
	require.True(t, base.Hostname() != "" && (base.Scheme == "http" || base.Scheme == "https") &&
		base.User == nil && base.RawQuery == "" && !base.ForceQuery && base.Fragment == "" &&
		(base.Path == "" || base.Path == "/"), "gateway URL must be an HTTP(S) root without credentials, query, or fragment")
	if base.Scheme == "http" {
		require.True(t, base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1" || base.Hostname() == "::1",
			"HTTP is supported only for loopback testing; remote gateways require HTTPS")
	}
	for name, key := range map[string]string{"primary": keys.Primary, "other merchant": keys.Other, "read-only": keys.ReadOnly} {
		require.True(t, strings.HasPrefix(key, "sk_") && len(key) > 3, "missing or invalid %s test credential", name)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return &client{
		http: &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		baseURL: strings.TrimRight(address, "/"), keys: keys,
	}
}

func (c *client) do(t *testing.T, input request) (result response, err error) {
	t.Helper()
	var body, data []byte
	var req *http.Request
	defer func() {
		if *httpVerbose {
			t.Logf("\n%s", formatExchange(t.Name(), input, req, body, result, data, err))
		}
	}()
	if raw, ok := input.body.(json.RawMessage); ok {
		body = raw
	} else if input.body != nil {
		body, err = json.Marshal(input.body)
		if err != nil {
			return response{}, fmt.Errorf("encode test request: %w", err)
		}
	}
	req, err = http.NewRequest(input.method, c.baseURL+input.path, bytes.NewReader(body))
	if err != nil {
		return response{}, fmt.Errorf("construct test request: %w", err)
	}
	if input.credential != "" {
		req.Header.Set("Authorization", "Bearer "+input.credential)
	}
	if input.key != "" {
		req.Header.Set("X-Idempotency-Key", input.key)
	}
	if input.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range input.headers {
		req.Header[name] = values
	}
	reply, err := c.http.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("send test request: %w", err)
	}
	defer reply.Body.Close()
	result = response{status: reply.StatusCode, headers: reply.Header}
	data, err = io.ReadAll(io.LimitReader(reply.Body, (1<<20)+1))
	if err != nil {
		return result, fmt.Errorf("read test response: %w", err)
	}
	if len(data) > 1<<20 {
		return result, fmt.Errorf("test response exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &result.body); err != nil {
		return result, fmt.Errorf("gateway returned non-object JSON (HTTP %d)", reply.StatusCode)
	}
	if result.body == nil {
		return result, fmt.Errorf("gateway returned null JSON (HTTP %d)", reply.StatusCode)
	}
	return result, nil
}

func (c *client) send(t *testing.T, input request, status int) response {
	t.Helper()
	// Keep sequential scenarios below the default 10/s merchant limit.
	if wait := 125*time.Millisecond - time.Since(c.lastSentAt); wait > 0 {
		time.Sleep(wait)
	}
	c.lastSentAt = time.Now()
	reply, err := c.do(t, input)
	require.NoError(t, err)
	require.Equal(t, status, reply.status, "%s %s (use -v -args -http-verbose for the development HTTP transcript)", input.method, input.path)
	reply.check(t)
	return reply
}

func (r response) check(t *testing.T) {
	t.Helper()
	expectedType := "application/json"
	if r.status >= 400 {
		expectedType = "application/problem+json"
		var status int
		require.NoError(t, json.Unmarshal(r.body["status"], &status))
		assert.Equal(t, r.status, status)
		assert.NotEmpty(t, r.string(t, "title"))
	}
	assert.Equal(t, expectedType, r.headers.Get("Content-Type"))
	id := r.string(t, "request_id")
	checkUUID(t, id)
	assert.Equal(t, id, r.headers.Get("X-Request-Id"))
	assert.Equal(t, "no-store", r.headers.Get("Cache-Control"))
	for _, field := range []string{"card_number", "cvv", "merchant_id", "idempotency_key", "request_fingerprint", "authorization_code"} {
		assert.NotContains(t, r.body, field)
	}
}

func checkUUID(t *testing.T, value string) {
	t.Helper()
	id, err := uuid.Parse(value)
	require.NoError(t, err, "identifier must be a UUID")
	assert.True(t, id.String() == value && id.Version() == 7, "gateway identifiers must be canonical UUIDv7")
}

func (r response) string(t *testing.T, key string) string {
	t.Helper()
	require.Contains(t, r.body, key)
	var value string
	require.NoError(t, json.Unmarshal(r.body[key], &value), "field %s must be a string", key)
	return value
}

func (r response) transactions(t *testing.T) []transaction {
	t.Helper()
	created, err := time.Parse(time.RFC3339Nano, r.string(t, "created_at"))
	require.NoError(t, err)
	updated, err := time.Parse(time.RFC3339Nano, r.string(t, "updated_at"))
	require.NoError(t, err)
	assert.False(t, created.IsZero())
	assert.False(t, updated.Before(created))
	if r.string(t, "status") == "Created" {
		assert.Equal(t, created, updated)
	}
	require.Contains(t, r.body, "bank_transactions")
	assert.NotEqual(t, "null", string(r.body["bank_transactions"]))
	var result []transaction
	require.NoError(t, json.Unmarshal(r.body["bank_transactions"], &result))
	var raw []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(r.body["bank_transactions"], &raw))
	for i, item := range result {
		checkUUID(t, item.ID)
		assert.Regexp(t, "^[0-9]{4}$", item.LastFour)
		assert.False(t, item.CreatedAt.IsZero())
		for _, field := range []string{"card_number", "cvv", "card_token", "merchant_id", "idempotency_key", "request_fingerprint", "authorization_code"} {
			assert.NotContains(t, raw[i], field)
		}
		if item.Status != "Scheduled" {
			require.NotNil(t, item.Completed)
			assert.False(t, item.Completed.Before(item.CreatedAt))
		}
	}
	return result
}

func validCard(ending string) map[string]interface{} {
	return map[string]interface{}{"card_number": "222240534324000" + ending, "expiry_month": 4,
		"expiry_year": time.Now().UTC().Year() + 10, "cvv": "012"}
}

func (c *client) create(t *testing.T, key string) response {
	t.Helper()
	return c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary,
		key: key, body: map[string]interface{}{"currency": "GBP", "amount": 100}}, http.StatusCreated)
}

func (c *client) createID(t *testing.T) string {
	t.Helper()
	result := c.create(t, uuid.NewString())
	id := result.string(t, "payment_intent_id")
	checkUUID(t, id)
	assert.Equal(t, "/api/PaymentIntents/"+id, result.headers.Get("Location"))
	assert.Equal(t, "Created", result.string(t, "status"))
	assert.Empty(t, result.transactions(t))
	return id
}

func (c *client) token(t *testing.T, ending string) string {
	t.Helper()
	return c.tokenFor(t, ending, c.keys.Primary)
}

func (c *client) tokenFor(t *testing.T, ending, credential string) string {
	t.Helper()
	start := time.Now().UTC()
	result := c.send(t, request{method: http.MethodPost, path: "/api/CardTokens", credential: credential,
		body: validCard(ending)}, http.StatusOK)
	token := result.string(t, "token")
	checkUUID(t, token)
	var expiry time.Time
	require.NoError(t, json.Unmarshal(result.body["expires_at"], &expiry))
	assert.True(t, expiry.After(start.Add(14*time.Minute)) && expiry.Before(time.Now().UTC().Add(16*time.Minute)),
		"token should expire approximately 15 minutes after issuance")
	assert.Len(t, result.body, 3)
	return token
}

func (c *client) confirm(t *testing.T, id, token, key string, status int) response {
	t.Helper()
	return c.send(t, confirmationRequest(id, token, key, c.keys.Primary), status)
}

func (c *client) concurrent(t *testing.T, inputs []request) []response {
	t.Helper()
	// Business-concurrency cases fit within one merchant quota, independent of setup traffic.
	time.Sleep(time.Second)
	results := c.burst(t, inputs)
	time.Sleep(time.Second)
	return results
}

func (c *client) burst(t *testing.T, inputs []request) []response {
	t.Helper()
	results := make([]response, len(inputs))
	errs := make([]error, len(inputs))
	start := make(chan struct{})
	var work sync.WaitGroup
	for i := range inputs {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			<-start
			input := inputs[i]
			input.description = fmt.Sprintf("Concurrent request %d/%d: %s", i+1, len(inputs), describeRequest(input))
			results[i], errs[i] = c.do(t, input)
		}(i)
	}
	close(start)
	work.Wait()
	for i := range results {
		require.NoError(t, errs[i], "concurrent request %d failed", i)
		results[i].check(t)
	}
	return results
}

func (c *client) get(t *testing.T, id string) response {
	t.Helper()
	result := c.send(t, request{method: http.MethodGet, path: "/api/PaymentIntents/" + id, credential: c.keys.Primary}, http.StatusOK)
	assert.Equal(t, id, result.string(t, "payment_intent_id"))
	assert.Equal(t, "GBP", result.string(t, "currency"))
	assert.JSONEq(t, "100", string(result.body["amount"]))
	result.transactions(t)
	return result
}
