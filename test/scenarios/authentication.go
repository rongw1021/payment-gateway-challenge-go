package scenarios

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func (c *client) authenticationAndAuthorization(t *testing.T) {
	id, token := c.createID(t), c.token(t, "7")
	for _, endpoint := range []struct {
		name  string
		input request
	}{
		{"Tokenize", request{method: http.MethodPost, path: "/api/CardTokens", body: validCard("7")}},
		{"Create", request{method: http.MethodPost, path: "/api/PaymentIntents", key: uuid.NewString(), body: intentDetails()}},
		{"Confirm", confirmationRequest(id, token, uuid.NewString(), "")},
		{"Retrieve", request{method: http.MethodGet, path: "/api/PaymentIntents/" + id}},
	} {
		runCase(t, endpoint.name, "Check authentication and scope enforcement for this endpoint.", func(t *testing.T) {
			for _, invalid := range []struct {
				name   string
				header []string
			}{
				{"MissingCredentials", nil}, {"UnknownAPIKey", []string{"Bearer sk_" + uuid.NewString()}},
				{"MissingBearerValue", []string{"Bearer"}}, {"EmptyKeyPayload", []string{"Bearer sk_"}},
				{"WrongScheme", []string{"Basic " + c.keys.Primary}},
				{"MissingKeyPrefix", []string{"Bearer invalid"}},
				{"DuplicateAuthorization", []string{"Bearer " + c.keys.Primary, "Bearer " + c.keys.Primary}},
			} {
				runCase(t, invalid.name, "Reject invalid credentials before processing the request.", func(t *testing.T) {
					input := endpoint.input
					input.headers = http.Header{"Authorization": invalid.header}
					reply := c.send(t, input, 401)
					assert.Equal(t, "Bearer", reply.headers.Get("WWW-Authenticate"))
				})
			}
			runCase(t, "ReadOnlyScope", "Read scope permits lookup but never bypasses merchant ownership or grants write permissions.", func(t *testing.T) {
				input := endpoint.input
				input.credential = c.keys.ReadOnly
				if input.method == http.MethodGet {
					c.get(t, id)
					c.send(t, input, 404)
				} else {
					c.send(t, input, 403)
				}
			})
		})
	}
	runCase(t, "AuthenticationBeforeValidation", "Invalid credentials take precedence over malformed input and a missing idempotency key.", func(t *testing.T) {
		c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", body: json.RawMessage("{")}, 401)
		c.send(t, request{method: http.MethodPost, path: "/api/PaymentIntents", credential: c.keys.ReadOnly,
			body: json.RawMessage("{")}, 403)
	})
	runCase(t, "RejectedWritesPreserveToken", "Authentication and scope failures do not consume the owner's token.", func(t *testing.T) {
		c.confirm(t, id, token, uuid.NewString(), 200)
	})
}
