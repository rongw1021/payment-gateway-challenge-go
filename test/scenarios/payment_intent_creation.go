package scenarios

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intentDetails() map[string]interface{} {
	return map[string]interface{}{"currency": "GBP", "amount": 100}
}

func (c *client) paymentIntentCreation(t *testing.T) {
	for _, test := range []struct {
		currency string
		amount   int64
	}{
		{"GBP", 1}, {"USD", 100}, {"EUR", 2147483647},
	} {
		runCase(t, "Valid"+test.currency, "Accept a supported currency and an amount boundary, returning Location and an empty history.", func(t *testing.T) {
			created := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary,
				key: uuid.NewString(), body: map[string]interface{}{"currency": test.currency, "amount": test.amount}}, 201)
			id := created.string(t, "payment_intent_id")
			checkUUID(t, id)
			assert.Equal(t, "/api/PaymentIntents/"+id, created.headers.Get("Location"))
			assert.Equal(t, "Created", created.string(t, "status"))
			assert.Equal(t, test.currency, created.string(t, "currency"))
			var amount int64
			require.NoError(t, json.Unmarshal(created.body["amount"], &amount))
			assert.Equal(t, test.amount, amount)
			assert.Empty(t, created.transactions(t))
			confirmed := c.confirm(t, id, c.token(t, "7"), uuid.NewString(), 200)
			assert.Equal(t, test.currency, confirmed.string(t, "currency"))
			assert.JSONEq(t, string(created.body["amount"]), string(confirmed.body["amount"]))
		})
	}
	runCase(t, "InvalidFields", "Invalid details must not reserve the creation key.", func(t *testing.T) {
		for _, test := range []struct {
			name, field string
			value       interface{}
			remove      bool
		}{
			{"MissingCurrency", "currency", nil, true}, {"NullCurrency", "currency", nil, false},
			{"EmptyCurrency", "currency", "", false}, {"UnsupportedCurrency", "currency", "JPY", false},
			{"LowercaseCurrency", "currency", "gbp", false}, {"MissingAmount", "amount", nil, true},
			{"NullAmount", "amount", nil, false}, {"ZeroAmount", "amount", 0, false},
			{"NegativeAmount", "amount", -1, false}, {"FractionalAmount", "amount", 1.5, false},
			{"StringAmount", "amount", "100", false}, {"OverflowAmount", "amount", int64(2147483648), false},
		} {
			runCase(t, test.name, "Reject the invalid details, then successfully use the same key with valid details.", func(t *testing.T) {
				body, key := intentDetails(), uuid.NewString()
				if test.remove {
					delete(body, test.field)
				} else {
					body[test.field] = test.value
				}
				c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary, key: key, body: body}, 400)
				created := c.create(t, key)
				assert.Empty(t, created.transactions(t))
			})
		}
	})
	runCase(t, "InvalidIdempotencyKey", "Require one canonical lowercase hyphenated key.", func(t *testing.T) {
		for _, test := range invalidKeys() {
			runCase(t, test.name, "Reject the invalid key before creating an intent.", func(t *testing.T) {
				reply := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary,
					body: intentDetails(), headers: http.Header{"X-Idempotency-Key": test.values}}, 400)
				validationField(t, reply, "idempotency_key")
			})
		}
	})
	runCase(t, "HTTPInput", "Reject invalid HTTP input without reserving the key.", func(t *testing.T) {
		for _, test := range invalidHTTPInputs() {
			runCase(t, test.name, "The same key remains usable after the rejected request.", func(t *testing.T) {
				key := uuid.NewString()
				c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary,
					key: key, body: test.inputBody(t, intentDetails()), headers: http.Header{"Content-Type": {test.contentType}}}, test.status)
				c.create(t, key)
			})
		}
	})
	runCase(t, "MatchingReplay", "Identical creation requests return one intent, the same Location, and fresh request IDs.", func(t *testing.T) {
		key := uuid.NewString()
		first, replay := c.create(t, key), c.create(t, key)
		assert.Equal(t, first.string(t, "payment_intent_id"), replay.string(t, "payment_intent_id"))
		assert.Equal(t, first.headers.Get("Location"), replay.headers.Get("Location"))
		assert.NotEqual(t, first.string(t, "request_id"), replay.string(t, "request_id"))
		assert.Empty(t, replay.transactions(t))
	})
	for _, test := range []struct {
		name, field string
		value       interface{}
	}{
		{"Currency", "currency", "USD"}, {"Amount", "amount", 101},
	} {
		runCase(t, "Conflicting"+test.name, "A changed fingerprint returns 409 and preserves the original intent.", func(t *testing.T) {
			key := uuid.NewString()
			first := c.create(t, key)
			body := intentDetails()
			body[test.field] = test.value
			c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Primary, key: key, body: body}, 409)
			c.expectState(t, first.string(t, "payment_intent_id"), "Created")
		})
	}
	for _, outcome := range bankOutcomes() {
		runCase(t, "ReplayAfter"+outcome.name, "Creation replay returns current state/history with HTTP 201.", func(t *testing.T) {
			key := uuid.NewString()
			first := c.create(t, key)
			id := first.string(t, "payment_intent_id")
			c.confirm(t, id, c.token(t, outcome.ending), uuid.NewString(), outcome.status)
			replay := c.create(t, key)
			current := c.expectState(t, id, outcome.intent, outcome.bank)
			assert.Equal(t, outcome.intent, replay.string(t, "status"))
			assert.Equal(t, first.headers.Get("Location"), replay.headers.Get("Location"))
			assert.JSONEq(t, string(current.body["bank_transactions"]), string(replay.body["bank_transactions"]))
		})
	}
	runCase(t, "MerchantScopedKey", "Different merchants may use the same creation key independently.", func(t *testing.T) {
		key := uuid.NewString()
		first := c.create(t, key)
		other := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Other, key: key, body: intentDetails()}, 201)
		assert.NotEqual(t, first.string(t, "payment_intent_id"), other.string(t, "payment_intent_id"))
	})
	runCase(t, "ConcurrentMatchingRequests", "Concurrent replays create one intent and retain per-request correlation.", func(t *testing.T) {
		key := uuid.NewString()
		inputs := make([]request, 8)
		for i := range inputs {
			inputs[i] = request{method: http.MethodPost, path: "/api/PaymentIntents", key: key, credential: c.keys.Primary, body: intentDetails()}
		}
		results := c.concurrent(t, inputs)
		id := results[0].string(t, "payment_intent_id")
		ids := make(map[string]bool)
		for _, result := range results {
			require.Equal(t, 201, result.status)
			assert.Equal(t, id, result.string(t, "payment_intent_id"))
			assert.Equal(t, "/api/PaymentIntents/"+id, result.headers.Get("Location"))
			requestID := result.string(t, "request_id")
			assert.False(t, ids[requestID])
			ids[requestID] = true
		}
		c.expectState(t, id, "Created")
	})
}
