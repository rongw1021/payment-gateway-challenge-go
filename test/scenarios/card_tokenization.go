package scenarios

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (c *client) cardTokenization(t *testing.T) {
	runCase(t, "ValidCard", "Issue a UUIDv7 token with a 15-minute expiry and no-store metadata.", func(t *testing.T) {
		c.token(t, "7")
	})
	runCase(t, "DistinctTokensForRepeatedDetails", "Tokenization is not idempotent: identical card details receive distinct tokens.", func(t *testing.T) {
		first, second := c.token(t, "7"), c.token(t, "7")
		assert.NotEqual(t, first, second)
	})
	for _, digits := range []int{14, 19} {
		runCase(t, fmt.Sprintf("PAN%dDigitsAndFourDigitCVV", digits), "Accept card/CVV boundaries and preserve the correct masked suffix on payment.", func(t *testing.T) {
			card := validCard("7")
			card["card_number"] = strings.Repeat("2", digits-1) + "7"
			card["cvv"] = "0123"
			result := c.send(t, request{method: http.MethodPost, path: "/api/CardTokens", credential: c.keys.Primary, body: card}, 200)
			checkUUID(t, result.string(t, "token"))
			paid := c.confirm(t, c.createID(t), result.string(t, "token"), uuid.NewString(), 200)
			history := paid.transactions(t)
			require.Len(t, history, 1)
			assert.Equal(t, "2227", history[0].LastFour)
		})
	}
	runCase(t, "InvalidFields", "Reject missing, malformed, out-of-range, and expired card details.", func(t *testing.T) {
		for _, test := range []struct {
			name, field, errorField string
			value                   interface{}
			remove                  bool
		}{
			{"MissingPAN", "card_number", "card_number", nil, true},
			{"NullPAN", "card_number", "card_number", nil, false},
			{"ShortPAN", "card_number", "card_number", "123", false},
			{"LongPAN", "card_number", "card_number", strings.Repeat("2", 20), false},
			{"NonnumericPAN", "card_number", "card_number", "222240534324000x", false},
			{"NonASCIIPAN", "card_number", "card_number", "222240534324000\u0667", false},
			{"NumericPAN", "card_number", "body", int64(2222405343240007), false},
			{"MissingMonth", "expiry_month", "expiry_month", nil, true},
			{"ZeroMonth", "expiry_month", "expiry_month", 0, false},
			{"MonthAbove12", "expiry_month", "expiry_month", 13, false},
			{"FractionalMonth", "expiry_month", "body", 4.5, false},
			{"MissingYear", "expiry_year", "expiry_year", nil, true},
			{"ZeroYear", "expiry_year", "expiry_year", 0, false},
			{"YearAbove9999", "expiry_year", "expiry_year", 10000, false},
			{"ExpiredCard", "expiry_year", "expiry_year", 2000, false},
			{"MissingCVV", "cvv", "cvv", nil, true},
			{"NullCVV", "cvv", "cvv", nil, false},
			{"ShortCVV", "cvv", "cvv", "12", false},
			{"LongCVV", "cvv", "cvv", "12345", false},
			{"NonnumericCVV", "cvv", "cvv", "xyz", false},
			{"NumericCVV", "cvv", "body", 123, false},
		} {
			runCase(t, test.name, "Reject this invalid card field with a validation error.", func(t *testing.T) {
				card := validCard("7")
				if test.remove {
					delete(card, test.field)
				} else {
					card[test.field] = test.value
				}
				reply := c.send(t, request{method: http.MethodPost, path: "/api/CardTokens", credential: c.keys.Primary, body: card}, 400)
				validationField(t, reply, test.errorField)
				assert.NotContains(t, reply.body, "token")
			})
		}
	})
	runCase(t, "HTTPInput", "Validate JSON shape, content type, and the request-body limit.", func(t *testing.T) {
		for _, test := range invalidHTTPInputs() {
			runCase(t, test.name, "Reject invalid HTTP input without issuing a token.", func(t *testing.T) {
				reply := c.send(t, request{method: http.MethodPost, path: "/api/CardTokens", credential: c.keys.Primary,
					body: test.inputBody(t, validCard("7")), headers: http.Header{"Content-Type": {test.contentType}}}, test.status)
				assert.NotContains(t, reply.body, "token")
			})
		}
	})
}
