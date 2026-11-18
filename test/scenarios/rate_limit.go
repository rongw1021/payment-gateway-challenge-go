package scenarios

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (c *client) rateLimit(t *testing.T) {
	// This shared HTTP probe targets the development fixture's 100/s limit.
	// Exact admission counts and cutoff boundaries use a fake clock in unit tests.
	time.Sleep(time.Second)
	inputs := make([]request, 200)
	path := "/api/PaymentIntents/" + uuid.NewString()
	for i := range inputs {
		credential := c.keys.Primary
		if i%4 >= 2 {
			credential = c.keys.Other
		}
		if i%2 == 0 {
			inputs[i] = request{method: http.MethodGet, path: path, credential: credential}
		} else {
			inputs[i] = request{method: http.MethodPost, path: "/api/CardTokens", credential: credential, body: map[string]interface{}{}}
		}
	}
	var rejected []response
	if !runCase(t, "ConcurrentBurstAcrossRoutesAndMerchants", "A bounded burst shares one quota and produces correlated 429 responses before normal endpoint processing.", func(t *testing.T) {
		results := c.concurrent(t, inputs)
		routes := make(map[string]bool)
		merchants := make(map[string]bool)
		for i, result := range results {
			if result.status == 429 {
				rejected = append(rejected, result)
				routes[inputs[i].path] = true
				merchants[inputs[i].credential] = true
				assert.Equal(t, "1", result.headers.Get("Retry-After"))
				assert.Empty(t, result.headers.Get("WWW-Authenticate"))
			} else if inputs[i].method == http.MethodGet {
				assert.Equal(t, 404, result.status)
			} else {
				assert.Equal(t, 400, result.status)
			}
		}
		require.NotEmpty(t, rejected, "no throttling observed; run against an isolated development gateway with the default 100/s limit")
		assert.Len(t, routes, 2, "both route families must share the exhausted quota")
		assert.Len(t, merchants, 2, "both merchants must share the exhausted quota")
	}) {
		return
	}
	runCase(t, "HealthCheckRemainsAvailable", "GET /ping remains available after the burst.", func(t *testing.T) {
		reply, err := c.do(t, request{method: http.MethodGet, path: "/ping"})
		require.NoError(t, err)
		assert.Equal(t, 200, reply.status)
		assert.Equal(t, "pong", reply.string(t, "message"))
		assert.Empty(t, reply.headers.Get("Retry-After"))
	})
	runCase(t, "RecoveryAfterRetryAfter", "Admission resumes after waiting the advertised one-second interval.", func(t *testing.T) {
		require.Equal(t, "1", rejected[0].headers.Get("Retry-After"))
		time.Sleep(time.Second)
		reply := c.send(t, request{method: http.MethodGet, path: path, credential: c.keys.Primary}, 404)
		assert.Empty(t, reply.headers.Get("Retry-After"))
	})
}
