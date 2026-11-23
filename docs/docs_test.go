package docs

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSwaggerArtifactsAgree(t *testing.T) {
	rawJSON, err := os.ReadFile("swagger.json")
	require.NoError(t, err)
	var document map[string]interface{}
	require.NoError(t, json.Unmarshal(rawJSON, &document))
	rawYAML, err := os.ReadFile("swagger.yaml")
	require.NoError(t, err)
	var yamlDocument map[string]interface{}
	require.NoError(t, yaml.Unmarshal(rawYAML, &yamlDocument))
	normalized, err := json.Marshal(yamlDocument)
	require.NoError(t, err)
	assert.JSONEq(t, string(rawJSON), string(normalized))

	var embedded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(SwaggerInfo.ReadDoc()), &embedded))
	for _, field := range []string{"swagger", "info", "basePath", "paths", "definitions", "securityDefinitions"} {
		assert.Equal(t, document[field], embedded[field], "embedded Swagger differs in %s", field)
	}
}

func TestSwaggerDocumentsCurrentHTTPContract(t *testing.T) {
	var document struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Host                string                     `json:"host"`
		SecurityDefinitions map[string]json.RawMessage `json:"securityDefinitions"`
		Paths               map[string]map[string]struct {
			Produces  []string              `json:"produces"`
			Security  []map[string][]string `json:"security"`
			Responses map[string]struct {
				Headers map[string]json.RawMessage `json:"headers"`
				Schema  struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"responses"`
		} `json:"paths"`
		Definitions map[string]struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Format   string `json:"format"`
				Nullable bool   `json:"x-nullable"`
			} `json:"properties"`
		} `json:"definitions"`
	}
	require.NoError(t, json.Unmarshal([]byte(SwaggerInfo.ReadDoc()), &document))
	assert.NotEmpty(t, document.Info.Version)
	assert.Empty(t, document.Host, "Swagger UI must use the current origin")
	assert.Len(t, document.SecurityDefinitions, 1)
	assert.Contains(t, document.SecurityDefinitions, "ApiKey")
	for _, endpoint := range []struct{ path, method string }{
		{"/api/CardTokens", "post"}, {"/api/PaymentIntents", "post"},
		{"/api/PaymentIntents/{paymentIntentId}", "get"}, {"/api/PaymentIntents/{paymentIntentId}/confirm", "post"},
	} {
		operation, exists := document.Paths[endpoint.path][endpoint.method]
		require.True(t, exists, "missing %s %s", endpoint.method, endpoint.path)
		assert.ElementsMatch(t, []string{"application/json", "application/problem+json"}, operation.Produces)
		require.Len(t, operation.Security, 1)
		assert.Contains(t, operation.Security[0], "ApiKey")
		require.Contains(t, operation.Responses, "429")
		assert.Contains(t, operation.Responses["429"].Headers, "Retry-After")
		require.Contains(t, operation.Responses, "401")
		assert.Contains(t, operation.Responses["401"].Headers, "WWW-Authenticate")
		if endpoint.method == "post" {
			require.Contains(t, operation.Responses, "408")
			assert.Equal(t, "#/definitions/models.ProblemDetails", operation.Responses["408"].Schema.Ref)
		}
		for status, response := range operation.Responses {
			for _, header := range []string{"X-Request-Id", "Cache-Control", "Pragma"} {
				assert.Contains(t, response.Headers, header, "%s %s response %s", endpoint.method, endpoint.path, status)
			}
		}
	}
	confirmation := document.Paths["/api/PaymentIntents/{paymentIntentId}/confirm"]["post"]
	require.Contains(t, confirmation.Responses, "202")
	assert.Equal(t, "#/definitions/models.PaymentIntentResponse", confirmation.Responses["202"].Schema.Ref)
	assert.Contains(t, confirmation.Responses["202"].Headers, "Location")
	assert.NotContains(t, confirmation.Responses, "503")
	assert.Empty(t, document.Paths["/ping"]["get"].Security)
	assert.NotContains(t, document.Paths["/ping"]["get"].Responses, "429")
	token := document.Definitions["models.CardTokenResponse"]
	assert.ElementsMatch(t, []string{"token", "expires_at", "request_id"}, token.Required)
	assert.Equal(t, "date-time", token.Properties["expires_at"].Format)
	transaction := document.Definitions["models.BankTransactionResponse"]
	assert.Equal(t, "date-time", transaction.Properties["created_at"].Format)
	assert.Equal(t, "date-time", transaction.Properties["completed_at"].Format)
	assert.True(t, transaction.Properties["completed_at"].Nullable)
	assert.Contains(t, transaction.Required, "completed_at", "the field is present even when its value is null")
	intent := document.Definitions["models.PaymentIntentResponse"]
	for _, field := range []string{"created_at", "updated_at"} {
		assert.Contains(t, intent.Required, field)
		assert.Equal(t, "date-time", intent.Properties[field].Format)
	}
}
