package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	original, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(directory))
	t.Cleanup(func() { require.NoError(t, os.Chdir(original)) })
	return directory
}

func validConfig(address string) string {
	hash := sha256.Sum256([]byte("test-payload"))
	return fmt.Sprintf(`server:
  address: %q
authentication:
  api_keys:
    - merchant_id: test-merchant
      key_hash: %q
      scopes: [card-tokens:create, payment-intents:create, payment-intents:confirm, payment-intents:read]
bank_simulator:
  base_address: "http://localhost:8080/"
  timeout: 5s
reconciliation:
  sweep_interval: 250ms
  bank_transaction_status_update_timeout: 2m
  batch_size: 100
observability:
  enabled: true
  service_name: test-gateway
  otlp:
    endpoint: "http://localhost:4318"
    timeout: 3s
  metrics:
    export_interval: 10s
rate_limit:
  requests_per_second: 25
`, address, hex.EncodeToString(hash[:]))
}

func TestLoadSelectsEnvironmentAndDecodesTypedConfiguration(t *testing.T) {
	directory := configDirectory(t)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.development.yaml"), []byte(validConfig(":8090")), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.staging.yaml"), []byte(validConfig(":9090")), 0600))
	t.Setenv("ENVIRONMENT", "development")
	require.NoError(t, os.Unsetenv("ENVIRONMENT"))
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ":8090", cfg.Server.Address)

	t.Setenv("ENVIRONMENT", "staging")
	// Legacy settings must not override the selected file.
	t.Setenv("GATEWAY_API_KEYS", "invalid-json")
	t.Setenv("BANK_SIMULATOR_BASE_ADDRESS", "invalid-url")
	t.Setenv("BANK_SIMULATOR_TIMEOUT", "invalid-duration")
	t.Setenv("RECONCILIATION_SWEEP_INTERVAL", "invalid-duration")
	t.Setenv("RECONCILIATION_BATCH_SIZE", "invalid-integer")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Server.Address)
	assert.Equal(t, "http://localhost:8080/", cfg.BankSimulator.BaseAddress)
	assert.Equal(t, 5*time.Second, cfg.BankSimulator.Timeout)
	assert.Equal(t, 250*time.Millisecond, cfg.Reconciliation.SweepInterval)
	assert.Equal(t, 2*time.Minute, cfg.Reconciliation.BankTransactionStatusUpdateTimeout)
	assert.EqualValues(t, 100, cfg.Reconciliation.BatchSize)
	assert.True(t, cfg.Observability.Enabled)
	assert.Equal(t, "test-gateway", cfg.Observability.ServiceName)
	assert.Equal(t, "http://localhost:4318", cfg.Observability.OTLP.Endpoint)
	assert.Equal(t, 10*time.Second, cfg.Observability.Metrics.ExportInterval)
	assert.Equal(t, 3*time.Second, cfg.Observability.OTLP.Timeout)
	assert.EqualValues(t, 25, cfg.RateLimit.RequestsPerSecond)
	auth, err := middleware.NewAuthenticator(cfg.Authentication.APIKeys)
	require.NoError(t, err)
	merchant, err := auth.Authenticate(http.Header{"Authorization": {"Bearer sk_test-payload"}})
	require.NoError(t, err)
	assert.Equal(t, "test-merchant", merchant.ID)
	assert.True(t, merchant.Allows(middleware.PaymentIntentsConfirm))
}

func TestLoadDefaultsMissingRateLimitForExistingProfiles(t *testing.T) {
	directory := configDirectory(t)
	t.Setenv("ENVIRONMENT", "test")
	content := strings.Replace(validConfig(":8090"), "rate_limit:\n  requests_per_second: 25\n", "", 1)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.test.yaml"), []byte(content), 0600))
	cfg, err := Load()
	require.NoError(t, err)
	assert.EqualValues(t, 100, cfg.RateLimit.RequestsPerSecond)
}

func TestLoadRejectsInvalidEnvironmentAndMissingSelectedFile(t *testing.T) {
	directory := configDirectory(t)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.development.yaml"), []byte(validConfig(":8090")), 0600))
	for _, environment := range []string{"", " ", "../production", `..\production`, "production.yaml", "Development"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("ENVIRONMENT", environment)
			cfg, err := Load()
			require.ErrorContains(t, err, "ENVIRONMENT")
			assert.Nil(t, cfg)
		})
	}
	t.Setenv("ENVIRONMENT", "production")
	cfg, err := Load()
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, cfg)
}

func TestLoadRejectsInvalidYAMLAndSchema(t *testing.T) {
	directory := configDirectory(t)
	t.Setenv("ENVIRONMENT", "test")
	valid := validConfig(":8090")
	for _, test := range []struct{ name, content string }{
		{"empty file", ""},
		{"null document", "null"},
		{"malformed YAML", "server: ["},
		{"unknown section", valid + "\nservre:\n  address: ':9000'\n"},
		{"unknown server field", strings.Replace(valid, "address:", "adress:", 1)},
		{"unknown bank field", strings.Replace(valid, "timeout: 5s", "timeuot: 5s", 1)},
		{"unknown credential field", strings.Replace(valid, "key_hash:", "api_key:", 1)},
		{"unknown reconciliation field", strings.Replace(valid, "batch_size:", "batch_szie:", 1)},
		{"unknown OTLP field", strings.Replace(valid, "endpoint:", "endpont:", 1)},
		{"unknown metric field", strings.Replace(valid, "export_interval:", "export_intervl:", 1)},
		{"unknown limiter field", strings.Replace(valid, "requests_per_second:", "request_per_second:", 1)},
		{"fractional rate", strings.Replace(valid, "requests_per_second: 25", "requests_per_second: 1.5", 1)},
		{"overflow rate", strings.Replace(valid, "requests_per_second: 25", "requests_per_second: 2147483648", 1)},
		{"legacy logging section", valid + "\nlogging:\n  elasticsearch:\n    enabled: false\n"},
		{"legacy telemetry section", valid + "\ntelemetry:\n  enabled: false\n"},
		{"duplicate key", strings.Replace(valid, `address: ":8090"`, "address: ':8090'\n  address: ':9000'", 1)},
		{"extra document", valid + "\n---\nserver:\n  address: ':9000'\n"},
		{"extra empty document", valid + "\n---\n"},
		{"invalid duration", strings.Replace(valid, "timeout: 5s", "timeout: someday", 1)},
		{"unitless duration", strings.Replace(valid, "timeout: 5s", "timeout: 5", 1)},
		{"fractional batch", strings.Replace(valid, "batch_size: 100", "batch_size: 1.5", 1)},
		{"overflow batch", strings.Replace(valid, "batch_size: 100", "batch_size: 2147483648", 1)},
		{"missing server", strings.Replace(valid, "server:\n  address: \":8090\"\n", "", 1)},
		{"invalid server address", strings.Replace(valid, `":8090"`, `"localhost"`, 1)},
		{"invalid server port", strings.Replace(valid, `":8090"`, `":70000"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(directory, "config.test.yaml"), []byte(test.content), 0600))
			cfg, err := Load()
			require.Error(t, err)
			assert.Nil(t, cfg)
		})
	}
}

func TestDecodedConfigurationUsesExistingStartupValidators(t *testing.T) {
	directory := configDirectory(t)
	t.Setenv("ENVIRONMENT", "test")
	valid := validConfig(":8090")
	for _, test := range []struct {
		name, content string
		validate      func(*Config) error
	}{
		{"missing credentials", "server:\n  address: ':8090'\nauthentication:\n  api_keys: []\n",
			func(cfg *Config) error {
				_, err := middleware.NewAuthenticator(cfg.Authentication.APIKeys)
				return err
			}},
		{"zero rate", strings.Replace(valid, "requests_per_second: 25", "requests_per_second: 0", 1),
			func(cfg *Config) error {
				_, err := middleware.NewRateLimiter(int(cfg.RateLimit.RequestsPerSecond))
				return err
			}},
		{"negative rate", strings.Replace(valid, "requests_per_second: 25", "requests_per_second: -1", 1),
			func(cfg *Config) error {
				_, err := middleware.NewRateLimiter(int(cfg.RateLimit.RequestsPerSecond))
				return err
			}},
		{"invalid bank URL", strings.Replace(valid, "http://localhost:8080/", "relative/", 1),
			func(cfg *Config) error {
				_, err := banksimulator.NewClient(cfg.BankSimulator.BaseAddress, cfg.BankSimulator.Timeout)
				return err
			}},
		{"missing bank timeout", strings.Replace(valid, "  timeout: 5s\n", "", 1),
			func(cfg *Config) error {
				_, err := banksimulator.NewClient(cfg.BankSimulator.BaseAddress, cfg.BankSimulator.Timeout)
				return err
			}},
		{"invalid cron interval", strings.Replace(valid, "sweep_interval: 250ms", "sweep_interval: 0s", 1),
			func(cfg *Config) error {
				return (backgroundservices.CronJobOptions{
					SweepInterval:                      cfg.Reconciliation.SweepInterval,
					BankTransactionStatusUpdateTimeout: cfg.Reconciliation.BankTransactionStatusUpdateTimeout,
					BatchSize:                          int(cfg.Reconciliation.BatchSize),
				}).Validate()
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(directory, "config.test.yaml"), []byte(test.content), 0600))
			cfg, err := Load()
			require.NoError(t, err)
			require.Error(t, test.validate(cfg))
		})
	}
}
