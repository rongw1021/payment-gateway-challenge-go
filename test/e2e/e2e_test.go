//go:build e2e

package e2e

import (
	"testing"

	"github.com/cko-recruitment/payment-gateway-challenge-go/test/scenarios"
	"github.com/stretchr/testify/require"
)

func TestE2EScenarios(t *testing.T) {
	cfg, err := loadConfig("config.e2e.yml")
	require.NoError(t, err, "configure config.e2e.yml beside this test for a running development gateway")
	keys := scenarios.Credentials{
		Primary:  cfg.APIKey,
		Other:    cfg.OtherAPIKey,
		ReadOnly: cfg.ReadOnlyAPIKey,
	}
	scenarios.Run(t, cfg.BaseURL, keys)
}
