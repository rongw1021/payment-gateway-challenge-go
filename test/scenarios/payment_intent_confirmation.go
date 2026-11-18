package scenarios

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func confirmationRequest(id, token, key, credential string) request {
	return request{method: http.MethodPost, path: "/api/PaymentIntents/" + id + "/confirm",
		key: key, credential: credential, body: map[string]string{"card_token": token}}
}

func (c *client) paymentIntentConfirmation(t *testing.T) {
	runCase(t, "InvalidIntentID", "Missing, malformed, and foreign intent IDs return 404 without consuming the token.", func(t *testing.T) {
		for _, name := range []string{"Malformed", "Missing", "ForeignMerchant"} {
			runCase(t, name, "The owner's token remains usable for a valid owned intent.", func(t *testing.T) {
				id := uuid.NewString()
				if name == "Malformed" {
					id = "not-a-guid"
				} else if name == "ForeignMerchant" {
					other := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents",
						credential: c.keys.Other, key: uuid.NewString(), body: intentDetails()}, 201)
					id = other.string(t, "payment_intent_id")
				}
				token, key := c.token(t, "7"), uuid.NewString()
				c.confirm(t, id, token, key, 404)
				c.confirm(t, c.createID(t), token, key, 200)
			})
		}
	})
	runCase(t, "InvalidCardToken", "Invalid tokens must not reserve an attempt or idempotency key.", func(t *testing.T) {
		for _, test := range []struct {
			name string
			body map[string]interface{}
		}{
			{"Missing", map[string]interface{}{}},
			{"Null", map[string]interface{}{"card_token": nil}},
			{"Empty", map[string]interface{}{"card_token": ""}},
			{"Blank", map[string]interface{}{"card_token": " "}},
			{"Malformed", map[string]interface{}{"card_token": "invalid"}},
			{"Uppercase", map[string]interface{}{"card_token": "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}},
			{"Numeric", map[string]interface{}{"card_token": 123}},
			{"Unknown", map[string]interface{}{"card_token": uuid.NewString()}},
		} {
			runCase(t, test.name, "Correcting the token permits the same key to create the first attempt.", func(t *testing.T) {
				id, key := c.createID(t), uuid.NewString()
				input := confirmationRequest(id, "", key, c.keys.Primary)
				input.body = test.body
				c.send(t, input, 400)
				c.expectState(t, id, "Created")
				c.confirm(t, id, c.token(t, "7"), key, 200)
			})
		}
	})
	runCase(t, "ConsumedCardToken", "A token already used by another intent cannot create another attempt.", func(t *testing.T) {
		token := c.token(t, "7")
		c.confirm(t, c.createID(t), token, uuid.NewString(), 200)
		id, key := c.createID(t), uuid.NewString()
		c.confirm(t, id, token, key, 400)
		c.expectState(t, id, "Created")
		c.confirm(t, id, c.token(t, "7"), key, 200)
	})
	runCase(t, "ForeignCardToken", "A rejected foreign token remains available to its owning merchant.", func(t *testing.T) {
		token := c.tokenFor(t, "7", c.keys.Other)
		id, key := c.createID(t), uuid.NewString()
		c.confirm(t, id, token, key, 400)
		c.expectState(t, id, "Created")
		other := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Other,
			key: uuid.NewString(), body: intentDetails()}, 201)
		c.send(t, confirmationRequest(other.string(t, "payment_intent_id"), token, key, c.keys.Other), 200)
	})
	runCase(t, "InvalidIdempotencyKey", "Invalid keys are rejected without consuming a valid token.", func(t *testing.T) {
		for _, test := range invalidKeys() {
			runCase(t, test.name, "A subsequent valid-key confirmation can still consume the token.", func(t *testing.T) {
				id, token := c.createID(t), c.token(t, "7")
				input := confirmationRequest(id, token, "", c.keys.Primary)
				input.headers = http.Header{"X-Idempotency-Key": test.values}
				reply := c.send(t, input, 400)
				validationField(t, reply, "idempotency_key")
				c.expectState(t, id, "Created")
				c.confirm(t, id, token, uuid.NewString(), 200)
			})
		}
	})
	runCase(t, "HTTPInput", "Invalid JSON and HTTP bodies must not reserve or consume anything.", func(t *testing.T) {
		for _, test := range invalidHTTPInputs() {
			runCase(t, test.name, "The same key and a valid token remain usable after the failed binding.", func(t *testing.T) {
				id, token, key := c.createID(t), c.token(t, "7"), uuid.NewString()
				input := confirmationRequest(id, token, key, c.keys.Primary)
				input.body = test.inputBody(t, map[string]string{"card_token": token})
				input.headers = http.Header{"Content-Type": {test.contentType}}
				c.send(t, input, test.status)
				c.expectState(t, id, "Created")
				c.confirm(t, id, token, key, 200)
			})
		}
	})
	for _, initial := range []string{"Created", "Declined"} {
		runCase(t, "From"+initial, "Exercise every supported bank outcome from a payable intent state.", func(t *testing.T) {
			for _, outcome := range bankOutcomes() {
				runCase(t, "To"+outcome.name, "Commit one new bank attempt and return its current payment state.", func(t *testing.T) {
					id := c.createID(t)
					var expected []string
					if initial == "Declined" {
						c.confirm(t, id, c.token(t, "2"), uuid.NewString(), 200)
						expected = append(expected, "Declined")
					}
					result := c.confirm(t, id, c.token(t, outcome.ending), uuid.NewString(), outcome.status)
					current := c.expectState(t, id, outcome.intent, append(expected, outcome.bank)...)
					c.checkConfirmation(t, result, current, outcome)
					history := current.transactions(t)
					assert.Equal(t, "000"+outcome.ending, history[len(history)-1].LastFour)
					assert.Equal(t, 4, history[len(history)-1].Month)
					assert.Equal(t, validCard(outcome.ending)["expiry_year"], history[len(history)-1].Year)
				})
			}
		})
	}
	for _, outcome := range []bankOutcome{bankOutcomes()[0], bankOutcomes()[2]} {
		runCase(t, "Already"+outcome.name, "A non-payable intent rejects a new attempt without consuming a token or key.", func(t *testing.T) {
			id, used := c.createID(t), c.token(t, outcome.ending)
			c.confirm(t, id, used, uuid.NewString(), outcome.status)
			key, unused := uuid.NewString(), c.token(t, "7")
			c.confirm(t, id, used, key, 409)
			c.confirm(t, id, unused, key, 409)
			c.expectState(t, id, outcome.intent, outcome.bank)
			c.confirm(t, c.createID(t), unused, key, 200)
		})
	}
	runCase(t, "ValidationPrecedence", "Header/path/binding checks take precedence over state and token availability.", func(t *testing.T) {
		input := confirmationRequest("not-a-guid", "", "", c.keys.Primary)
		input.body = json.RawMessage("{")
		reply := c.send(t, input, 400)
		validationField(t, reply, "idempotency_key")
		input.key = uuid.NewString()
		c.send(t, input, 404)
		id := c.createID(t)
		c.confirm(t, id, c.token(t, "7"), uuid.NewString(), 200)
		c.confirm(t, id, "malformed-token", uuid.NewString(), 400)
		c.expectState(t, id, "Authorized", "Authorized")
	})
	for _, outcome := range bankOutcomes() {
		runCase(t, "ImmediateReplay"+outcome.name, "A matching replay returns the same current attempt without another submission.", func(t *testing.T) {
			id, token, key := c.createID(t), c.token(t, outcome.ending), uuid.NewString()
			first := c.confirm(t, id, token, key, outcome.status)
			replay := c.confirm(t, id, token, key, outcome.status)
			current := c.expectState(t, id, outcome.intent, outcome.bank)
			c.checkConfirmation(t, first, current, outcome)
			c.checkConfirmation(t, replay, current, outcome)
			assert.NotEqual(t, first.string(t, "request_id"), replay.string(t, "request_id"))
		})
		runCase(t, "HistoricalReplayAfter"+outcome.name, "Replaying an old declined confirmation follows the current state and full history.", func(t *testing.T) {
			id, oldToken, oldKey := c.createID(t), c.token(t, "2"), uuid.NewString()
			c.confirm(t, id, oldToken, oldKey, 200)
			c.confirm(t, id, c.token(t, outcome.ending), uuid.NewString(), outcome.status)
			replay := c.confirm(t, id, oldToken, oldKey, outcome.status)
			current := c.expectState(t, id, outcome.intent, "Declined", outcome.bank)
			c.checkConfirmation(t, replay, current, outcome)
		})
	}
	runCase(t, "ConflictingIdempotencyKey", "A different token or intent conflicts in every stable state, including payable Declined.", func(t *testing.T) {
		for _, outcome := range bankOutcomes() {
			runCase(t, outcome.name, "Fingerprint conflicts take precedence over replay and new-attempt handling.", func(t *testing.T) {
				for _, changed := range []string{"Token", "Intent"} {
					runCase(t, "Changed"+changed, "The fingerprint binds both token and intent to the confirmation key.", func(t *testing.T) {
						id, token, key := c.createID(t), c.token(t, outcome.ending), uuid.NewString()
						c.confirm(t, id, token, key, outcome.status)
						if changed == "Token" {
							unused := c.token(t, "7")
							c.confirm(t, id, unused, key, 409)
							c.confirm(t, c.createID(t), unused, uuid.NewString(), 200)
						} else {
							other := c.createID(t)
							c.confirm(t, other, token, key, 409)
							c.expectState(t, other, "Created")
						}
						c.expectState(t, id, outcome.intent, outcome.bank)
					})
				}
			})
		}
	})
	runCase(t, "InvalidTokenPreservesDeclinedHistory", "An unavailable token must not erase a previous decline or reserve a retry key.", func(t *testing.T) {
		id, used := c.createID(t), c.token(t, "2")
		c.confirm(t, id, used, uuid.NewString(), 200)
		key := uuid.NewString()
		c.confirm(t, id, used, key, 400)
		c.expectState(t, id, "Declined", "Declined")
		c.confirm(t, id, c.token(t, "7"), key, 200)
		c.expectState(t, id, "Authorized", "Declined", "Authorized")
	})
	runCase(t, "SeparateCreationAndConfirmationKeyNamespaces", "The creation key can also identify the first confirmation.", func(t *testing.T) {
		key := uuid.NewString()
		id := c.create(t, key).string(t, "payment_intent_id")
		c.confirm(t, id, c.token(t, "7"), key, 200)
		c.expectState(t, id, "Authorized", "Authorized")
	})
	runCase(t, "MerchantScopedConfirmationKey", "Different merchants can reuse a confirmation key independently.", func(t *testing.T) {
		key := uuid.NewString()
		c.confirm(t, c.createID(t), c.token(t, "7"), key, 200)
		other := c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.Other,
			key: uuid.NewString(), body: intentDetails()}, 201)
		c.send(t, confirmationRequest(other.string(t, "payment_intent_id"), c.tokenFor(t, "7", c.keys.Other), key, c.keys.Other), 200)
	})
	runCase(t, "ConcurrentMatchingRequests", "Matching concurrent confirmations share exactly one attempt.", c.concurrentConfirmations)
	runCase(t, "ConcurrentDifferentKeys", "Only one new-key attempt can authorize an intent; the losing token remains available.", c.competingConfirmationKeys)
	runCase(t, "ConcurrentTokenConsumption", "Only one of two intents may consume a shared token.", c.concurrentTokenConsumption)
}

func (c *client) concurrentConfirmations(t *testing.T) {
	id, token, key := c.createID(t), c.token(t, "7"), uuid.NewString()
	inputs := make([]request, 8)
	for i := range inputs {
		inputs[i] = confirmationRequest(id, token, key, c.keys.Primary)
	}
	results := c.concurrent(t, inputs)
	current := c.expectState(t, id, "Authorized", "Authorized")
	history := current.transactions(t)
	for _, result := range results {
		require.Contains(t, []int{200, 409}, result.status)
		if result.status == 409 {
			assert.Equal(t, "Processing", result.string(t, "payment_intent_status"))
			assert.Equal(t, "Scheduled", result.string(t, "bank_transaction_status"))
			assert.Equal(t, history[0].ID, result.string(t, "bank_transaction_id"))
		} else {
			assert.JSONEq(t, string(current.body["bank_transactions"]), string(result.body["bank_transactions"]))
		}
	}
	c.confirm(t, id, token, key, 200)
}

func (c *client) competingConfirmationKeys(t *testing.T) {
	id := c.createID(t)
	tokens := []string{c.token(t, "7"), c.token(t, "7")}
	inputs := []request{
		confirmationRequest(id, tokens[0], uuid.NewString(), c.keys.Primary),
		confirmationRequest(id, tokens[1], uuid.NewString(), c.keys.Primary),
	}
	results := c.concurrent(t, inputs)
	assert.ElementsMatch(t, []int{200, 409}, []int{results[0].status, results[1].status})
	c.expectState(t, id, "Authorized", "Authorized")
	for i, result := range results {
		if result.status == 409 {
			c.confirm(t, c.createID(t), tokens[i], inputs[i].key, 200)
		}
	}
}

func (c *client) concurrentTokenConsumption(t *testing.T) {
	token := c.token(t, "7")
	ids := []string{c.createID(t), c.createID(t)}
	results := c.concurrent(t, []request{
		confirmationRequest(ids[0], token, uuid.NewString(), c.keys.Primary),
		confirmationRequest(ids[1], token, uuid.NewString(), c.keys.Primary),
	})
	assert.ElementsMatch(t, []int{200, 400}, []int{results[0].status, results[1].status})
	for i, result := range results {
		if result.status == 200 {
			c.expectState(t, ids[i], "Authorized", "Authorized")
		} else {
			c.expectState(t, ids[i], "Created")
		}
	}
}
