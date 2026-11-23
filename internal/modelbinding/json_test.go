package modelbinding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindJSONValidatesCardTokenDetails(t *testing.T) {
	for _, test := range []struct {
		name, field string
		change      func(*models.CreateCardTokenRequest)
	}{
		{"missing number", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "" }},
		{"too short to mask", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "123" }},
		{"short number", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "1234567890123" }},
		{"long number", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "12345678901234567890" }},
		{"non-digit number", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "022240534324000x" }},
		{"unicode number", "card_number", func(c *models.CreateCardTokenRequest) { c.CardNumber = "\u0661222405343240007" }},
		{"missing month", "expiry_month", func(c *models.CreateCardTokenRequest) { c.ExpiryMonth = 0 }},
		{"large month", "expiry_month", func(c *models.CreateCardTokenRequest) { c.ExpiryMonth = 13 }},
		{"missing year", "expiry_year", func(c *models.CreateCardTokenRequest) { c.ExpiryYear = 0 }},
		{"large year", "expiry_year", func(c *models.CreateCardTokenRequest) { c.ExpiryYear = 10000 }},
		{"missing CVV", "cvv", func(c *models.CreateCardTokenRequest) { c.Cvv = "" }},
		{"short CVV", "cvv", func(c *models.CreateCardTokenRequest) { c.Cvv = "12" }},
		{"long CVV", "cvv", func(c *models.CreateCardTokenRequest) { c.Cvv = "12345" }},
		{"non-digit CVV", "cvv", func(c *models.CreateCardTokenRequest) { c.Cvv = "12x" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 2035, Cvv: "012"}
			test.change(&card)
			body, err := json.Marshal(card)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			model, ok, err := BindJSON[models.CreateCardTokenRequest](response, request)
			require.NoError(t, err)

			assert.False(t, ok)
			assert.Nil(t, model)
			require.Equal(t, http.StatusBadRequest, response.Code)
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.NotEmpty(t, problem.Errors[test.field])
		})
	}
}

func TestBindJSONAcceptsCardNumberAndCVVLengthBoundaries(t *testing.T) {
	for _, number := range []string{"01234567890123", "0123456789012345678"} {
		for _, cvv := range []string{"012", "0123"} {
			card := models.CreateCardTokenRequest{CardNumber: number, ExpiryMonth: 4, ExpiryYear: 2035, Cvv: cvv}
			body, err := json.Marshal(card)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			model, ok, err := BindJSON[models.CreateCardTokenRequest](response, request)
			require.NoError(t, err)

			require.True(t, ok)
			require.NotNil(t, model)
			assert.Equal(t, card, *model)
			assert.Empty(t, response.Body.String())
		}
	}
}

func TestBindJSONValidatesPaymentIntent(t *testing.T) {
	for _, test := range []struct {
		name, body string
		fields     []string
	}{
		{"missing fields", `{}`, []string{"currency", "amount"}},
		{"unsupported currency", `{"currency":"JPY","amount":100}`, []string{"currency"}},
		{"lowercase currency", `{"currency":"usd","amount":100}`, []string{"currency"}},
		{"missing amount", `{"currency":"EUR"}`, []string{"amount"}},
		{"null amount", `{"currency":"GBP","amount":null}`, []string{"amount"}},
		{"zero amount", `{"currency":"GBP","amount":0}`, []string{"amount"}},
		{"negative amount", `{"currency":"GBP","amount":-1}`, []string{"amount"}},
		{"minimum integer", `{"currency":"GBP","amount":-2147483648}`, []string{"amount"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			model, ok, err := BindJSON[models.CreatePaymentIntentRequest](response, request)
			require.NoError(t, err)

			assert.False(t, ok)
			assert.Nil(t, model)
			require.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, http.StatusBadRequest, problem.Status)
			assert.Len(t, problem.Errors, len(test.fields))
			for _, field := range test.fields {
				assert.NotEmpty(t, problem.Errors[field])
			}
		})
	}
}

func TestBindJSONPreservesValidPaymentIntentAmounts(t *testing.T) {
	for _, test := range []struct {
		currency string
		amount   int32
	}{{"GBP", 1}, {"USD", 2147483647}, {"EUR", 100}} {
		t.Run(fmt.Sprintf("%s/%d", test.currency, test.amount), func(t *testing.T) {
			body := fmt.Sprintf(`{"currency":%q,"amount":%d}`, test.currency, test.amount)
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			model, ok, err := BindJSON[models.CreatePaymentIntentRequest](response, request)
			require.NoError(t, err)

			require.True(t, ok)
			require.NotNil(t, model)
			require.NotNil(t, model.Amount)
			assert.Equal(t, test.currency, model.Currency)
			assert.Equal(t, test.amount, *model.Amount)
			assert.Empty(t, response.Body.String())
		})
	}
}

func TestBindJSONValidatesConfirmationToken(t *testing.T) {
	const token = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"missing token", `{}`, false},
		{"null token", `{"card_token":null}`, false},
		{"empty token", `{"card_token":""}`, false},
		{"legacy token", fmt.Sprintf(`{"card_token":%q}`, strings.Repeat("A", 64)), false},
		{"uppercase", fmt.Sprintf(`{"card_token":%q}`, strings.ToUpper(token)), false},
		{"no hyphens", fmt.Sprintf(`{"card_token":%q}`, strings.ReplaceAll(token, "-", "")), false},
		{"braces", fmt.Sprintf(`{"card_token":%q}`, "{"+token+"}"), false},
		{"URN", fmt.Sprintf(`{"card_token":%q}`, "urn:uuid:"+token), false},
		{"canonical token", fmt.Sprintf(`{"card_token":%q}`, token), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			model, ok, err := BindJSON[models.ConfirmPaymentIntentRequest](response, request)
			require.NoError(t, err)

			assert.Equal(t, test.valid, ok)
			if test.valid {
				require.NotNil(t, model)
				assert.Equal(t, token, model.CardToken)
				assert.Empty(t, response.Body.String())
				return
			}
			assert.Nil(t, model)
			require.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			var problem models.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, []string{"Card token must be a lowercase hyphenated GUID."}, problem.Errors["card_token"])
		})
	}
}
