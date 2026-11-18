package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCredential() APIKeyCredential {
	hash := sha256.Sum256([]byte("test-payload"))
	return APIKeyCredential{
		MerchantID: "merchant",
		KeyHash:    hex.EncodeToString(hash[:]),
		Scopes:     []string{CardTokensCreate},
	}
}

func TestAPIKeyAuthentication(t *testing.T) {
	config := testCredential()
	auth, err := NewAuthenticator([]APIKeyCredential{config})
	require.NoError(t, err)
	// Configuration changes must not grant permissions after construction.
	config.Scopes[0] = "payment-intents:read"

	for _, header := range []string{"Bearer sk_test-payload", "bearer sk_test-payload", "Bearer  sk_test-payload"} {
		merchant, err := auth.Authenticate(http.Header{"Authorization": {header}})
		require.NoError(t, err)
		assert.Equal(t, "merchant", merchant.ID)
		assert.True(t, merchant.Allows(CardTokensCreate))
		assert.False(t, merchant.Allows("payment-intents:read"))
	}

	for _, values := range [][]string{
		nil, {""}, {"Basic sk_test-payload"}, {"Bearer"}, {"Bearer sk_"},
		{"Bearer test-payload"}, {"Bearer SK_test-payload"}, {"Bearer sk_unknown"},
		{"Bearer sk_test_payload"}, {"Bearer sk_test-payload,sk_other"},
		{"Bearer sk_test- payload"}, {"Bearer sk_test-\u00a0payload"},
		{"Bearer sk_test-payload", "Bearer sk_test-payload"},
	} {
		t.Run(strings.Join(values, "|"), func(t *testing.T) {
			_, err := auth.Authenticate(http.Header{"Authorization": values})
			assert.ErrorIs(t, err, ErrUnauthorized)
		})
	}
}

func TestAPIKeyConfigurationValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*APIKeyCredential)
	}{
		{"missing merchant", func(c *APIKeyCredential) { c.MerchantID = " " }},
		{"missing hash", func(c *APIKeyCredential) { c.KeyHash = "" }},
		{"invalid hex", func(c *APIKeyCredential) { c.KeyHash = strings.Repeat("z", 64) }},
		{"short hash", func(c *APIKeyCredential) { c.KeyHash = "ab" }},
		{"missing scopes", func(c *APIKeyCredential) { c.Scopes = nil }},
		{"unknown scope", func(c *APIKeyCredential) { c.Scopes = []string{"cards:*"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testCredential()
			test.change(&c)
			_, err := NewAuthenticator([]APIKeyCredential{c})
			require.Error(t, err)
		})
	}
	_, err := NewAuthenticator(nil)
	require.Error(t, err)
	first, duplicate := testCredential(), testCredential()
	duplicate.KeyHash = strings.ToUpper(duplicate.KeyHash)
	_, err = NewAuthenticator([]APIKeyCredential{first, duplicate})
	require.ErrorContains(t, err, "unique")

	for _, scopes := range [][]string{
		{}, {"payment-intents:read", "payment-intents:create", "payment-intents:confirm"},
	} {
		c := testCredential()
		c.Scopes = scopes
		auth, err := NewAuthenticator([]APIKeyCredential{c})
		require.NoError(t, err)
		merchant, err := auth.Authenticate(http.Header{"Authorization": {"Bearer sk_test-payload"}})
		require.NoError(t, err)
		assert.False(t, merchant.Allows(CardTokensCreate))
	}
}
