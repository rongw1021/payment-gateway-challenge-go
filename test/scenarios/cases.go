package scenarios

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bankOutcome struct {
	name, ending, intent, bank string
	status                     int
}

func bankOutcomes() []bankOutcome {
	return []bankOutcome{
		{"Authorized", "7", "Authorized", "Authorized", 200},
		{"Declined", "2", "Declined", "Declined", 200},
		{"PendingVerification", "0", "PendingVerification", "ServiceUnavailable", 202},
	}
}

func invalidKeys() []struct {
	name   string
	values []string
} {
	first, second := uuid.NewString(), uuid.NewString()
	return []struct {
		name   string
		values []string
	}{
		{"Missing", nil}, {"Empty", []string{""}}, {"Blank", []string{" "}},
		{"Malformed", []string{"not-a-uuid"}}, {"Combined", []string{first + "," + second}},
		{"Uppercase", []string{"01234567-89AB-4CDE-8123-456789ABCDEF"}},
		{"CompactUUID", []string{"0123456789ab4cde8123456789abcdef"}},
		{"Duplicate", []string{first, second}}, {"RepeatedIdenticalHeader", []string{first, first}},
	}
}

type httpInputCase struct {
	name, contentType, body string
	appendToValid           bool
	status                  int
}

func invalidHTTPInputs() []httpInputCase {
	return []httpInputCase{
		{"EmptyBody", "application/json", "", false, 400},
		{"MalformedJSON", "application/json", "{", false, 400},
		{"NullJSON", "application/json", "null", false, 400},
		{"ArrayJSON", "application/json", "[]", false, 400},
		{"ScalarJSON", "application/json", "true", false, 400},
		{"MultipleJSONValues", "application/json", " {}", true, 400},
		{"TrailingGarbage", "application/json", "garbage", true, 400},
		{"MissingContentType", "", "", true, 415},
		{"WrongContentType", "text/plain", "", true, 415},
		{"OversizedBody", "application/json", strings.Repeat(" ", 1<<20), true, 413},
	}
}

func (test httpInputCase) inputBody(t *testing.T, valid interface{}) json.RawMessage {
	t.Helper()
	if !test.appendToValid {
		return json.RawMessage(test.body)
	}
	body, err := json.Marshal(valid)
	require.NoError(t, err)
	return append(body, test.body...)
}

func validationField(t *testing.T, reply response, field string) {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(reply.body["errors"], &fields))
	assert.Contains(t, fields, field)
}

func (c *client) expectState(t *testing.T, id, status string, statuses ...string) response {
	t.Helper()
	payment := c.get(t, id)
	assert.Equal(t, status, payment.string(t, "status"))
	history := payment.transactions(t)
	require.Len(t, history, len(statuses))
	for i, expected := range statuses {
		assert.Equal(t, expected, history[i].Status)
		if i > 0 {
			assert.False(t, history[i].CreatedAt.Before(history[i-1].CreatedAt))
			assert.NotEqual(t, history[i-1].ID, history[i].ID)
		}
	}
	return payment
}

func (c *client) checkConfirmation(t *testing.T, result, current response, outcome bankOutcome) {
	t.Helper()
	history := current.transactions(t)
	require.NotEmpty(t, history)
	assert.Equal(t, outcome.intent, result.string(t, "status"))
	assert.Equal(t, current.string(t, "payment_intent_id"), result.string(t, "payment_intent_id"))
	assert.Equal(t, current.string(t, "currency"), result.string(t, "currency"))
	assert.JSONEq(t, string(current.body["amount"]), string(result.body["amount"]))
	assert.JSONEq(t, string(current.body["bank_transactions"]), string(result.body["bank_transactions"]))
	assert.Equal(t, current.string(t, "created_at"), result.string(t, "created_at"))
	assert.Equal(t, current.string(t, "updated_at"), result.string(t, "updated_at"))
	result.transactions(t)
	if outcome.status == http.StatusAccepted {
		assert.Equal(t, "/api/PaymentIntents/"+current.string(t, "payment_intent_id"), result.headers.Get("Location"))
	}
}
