package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validE2EConfig = `base_url: "http://localhost:8090"
api_key: "sk_test-primary"
other_api_key: "sk_test-other"
read_only_api_key: "sk_test-readonly"
`

func TestLoadE2EConfigReadsFileWithoutEnvironmentOverrides(t *testing.T) {
	t.Setenv("PAYMENT_GATEWAY_E2E_BASE_URL", "http://unused.example")
	t.Setenv("PAYMENT_GATEWAY_E2E_API_KEY", "sk_unused")
	path := filepath.Join(t.TempDir(), "config.e2e.yml")
	require.NoError(t, os.WriteFile(path, []byte(validE2EConfig), 0600))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8090", cfg.BaseURL)
	assert.Equal(t, "sk_test-primary", cfg.APIKey)
	assert.Equal(t, "sk_test-other", cfg.OtherAPIKey)
	assert.Equal(t, "sk_test-readonly", cfg.ReadOnlyAPIKey)
}

func TestLoadE2EConfigRejectsInvalidFilesWithoutLeakingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.e2e.yml")
	_, err := loadConfig(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, test := range []struct{ name, content string }{
		{"empty", ""},
		{"null", "null"},
		{"malformed", "api_key: [private-value"},
		{"wrong field type", strings.Replace(validE2EConfig, `"sk_test-primary"`, `{secret: private-value}`, 1)},
		{"unknown field", validE2EConfig + "apikey: private-value\n"},
		{"duplicate field", validE2EConfig + "api_key: private-value\n"},
		{"extra document", validE2EConfig + "---\napi_key: private-value\n"},
		{"extra empty document", validE2EConfig + "---\n"},
		{"missing URL", strings.Replace(validE2EConfig, "base_url: \"http://localhost:8090\"\n", "", 1)},
		{"missing key", strings.Replace(validE2EConfig, "api_key: \"sk_test-primary\"\n", "", 1)},
		{"blank other key", strings.Replace(validE2EConfig, `"sk_test-other"`, `" "`, 1)},
		{"null read-only key", strings.Replace(validE2EConfig, `"sk_test-readonly"`, "null", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(test.content), 0600))
			cfg, err := loadConfig(path)
			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.NotContains(t, err.Error(), "private-value")
		})
	}
}
