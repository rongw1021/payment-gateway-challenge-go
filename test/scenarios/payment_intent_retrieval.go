package scenarios

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func (c *client) paymentIntentRetrieval(t *testing.T) {
	runCase(t, "CreatedIntent", "Retrieve currency, amount, current state, and a non-null empty history.", func(t *testing.T) {
		c.expectState(t, c.createID(t), "Created")
	})
	for _, outcome := range bankOutcomes() {
		runCase(t, outcome.name+"Intent", "Retrieve a completed or pending attempt with masked card details.", func(t *testing.T) {
			id := c.createID(t)
			c.confirm(t, id, c.token(t, outcome.ending), uuid.NewString(), outcome.status)
			current := c.expectState(t, id, outcome.intent, outcome.bank)
			assert.Equal(t, "000"+outcome.ending, current.transactions(t)[0].LastFour)
		})
	}
	runCase(t, "CompleteOrderedHistory", "Multiple declined attempts and the final authorization all remain visible in order.", func(t *testing.T) {
		id := c.createID(t)
		for _, ending := range []string{"2", "2", "7"} {
			c.confirm(t, id, c.token(t, ending), uuid.NewString(), 200)
		}
		first := c.expectState(t, id, "Authorized", "Declined", "Declined", "Authorized")
		second := c.get(t, id)
		assert.NotEqual(t, first.string(t, "request_id"), second.string(t, "request_id"))
		assert.JSONEq(t, string(first.body["bank_transactions"]), string(second.body["bank_transactions"]))
	})
	for _, name := range []string{"MalformedID", "MissingID"} {
		runCase(t, name, "Unresolvable IDs return 404, not a validation or server error.", func(t *testing.T) {
			id := uuid.NewString()
			if name == "MalformedID" {
				id = "not-a-guid"
			}
			c.send(t, request{method: http.MethodGet, path: "/api/PaymentIntents/" + id, credential: c.keys.Primary}, 404)
		})
	}
	runCase(t, "ForeignMerchant", "Neither another full-access merchant nor a foreign read-only merchant can retrieve the intent.", func(t *testing.T) {
		id := c.createID(t)
		c.get(t, id)
		for _, credential := range []string{c.keys.Other, c.keys.ReadOnly} {
			c.send(t, request{method: http.MethodGet, path: "/api/PaymentIntents/" + id, credential: credential}, 404)
		}
	})
	runCase(t, "CanonicalizesValidUUIDPath", "Accepted UUID path representations return the canonical stored identifier.", func(t *testing.T) {
		id := c.createID(t)
		for _, pathID := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", "")} {
			result := c.send(t, request{method: http.MethodGet, path: "/api/PaymentIntents/" + pathID, credential: c.keys.Primary}, 200)
			assert.Equal(t, id, result.string(t, "payment_intent_id"))
		}
	})
}
