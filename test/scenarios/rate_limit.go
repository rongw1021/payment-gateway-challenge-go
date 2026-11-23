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
	path := "/api/PaymentIntents/" + uuid.NewString()
	runCase(t, "PerMerchantBurstAndIsolation", "Exceed one merchant's 10/s quota while another merchant can still use the gateway.", func(t *testing.T) {
		time.Sleep(time.Second)
		inputs := make([]request, 20)
		for i := range inputs {
			inputs[i] = request{method: http.MethodGet, path: path, credential: c.keys.Primary}
			if i%2 != 0 {
				inputs[i] = request{method: http.MethodPost, path: "/api/CardTokens", credential: c.keys.Primary, body: map[string]interface{}{}}
			}
		}
		rejected := 0
		for i, result := range c.burst(t, inputs) {
			if result.status == http.StatusTooManyRequests {
				rejected++
				assert.Equal(t, "1", result.headers.Get("Retry-After"))
			} else if inputs[i].method == http.MethodGet {
				assert.Equal(t, http.StatusNotFound, result.status)
			} else {
				assert.Equal(t, http.StatusBadRequest, result.status)
			}
		}
		require.Positive(t, rejected, "no merchant throttling observed; use the default 10/s merchant limit")
		c.send(t, request{method: http.MethodGet, path: path, credential: c.keys.Other}, http.StatusNotFound)
		time.Sleep(time.Second)
		c.send(t, request{method: http.MethodGet, path: path, credential: c.keys.Primary}, http.StatusNotFound)
	})

	// Unauthenticated traffic bypasses merchant quotas and isolates the coarse limit.
	time.Sleep(time.Second)
	inputs := make([]request, 200)
	for i := range inputs {
		if i%2 == 0 {
			inputs[i] = request{method: http.MethodGet, path: path}
		} else {
			inputs[i] = request{method: http.MethodPost, path: "/api/CardTokens", body: map[string]interface{}{}}
		}
	}
	var rejected []response
	if !runCase(t, "GlobalBurstBeforeAuthentication", "The global 100/s quota rejects excess traffic before authentication.", func(t *testing.T) {
		results := c.burst(t, inputs)
		for _, result := range results {
			if result.status == 429 {
				rejected = append(rejected, result)
				assert.Equal(t, "1", result.headers.Get("Retry-After"))
				assert.Empty(t, result.headers.Get("WWW-Authenticate"))
			} else {
				assert.Equal(t, http.StatusUnauthorized, result.status)
			}
		}
		require.NotEmpty(t, rejected, "no throttling observed; run against an isolated development gateway with the default 100/s limit")
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
