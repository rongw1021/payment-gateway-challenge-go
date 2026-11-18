package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/api"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/test/scenarios"
	"github.com/stretchr/testify/require"
)

func TestIntegrationScenarios(t *testing.T) {
	var gatewayLogs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&gatewayLogs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		if t.Failed() {
			t.Logf("Integration gateway logs:\n%s", gatewayLogs.String())
		}
	})
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/payments" {
			t.Errorf("unexpected bank request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			CardNumber string `json:"card_number"`
			ExpiryDate string `json:"expiry_date"`
			Currency   string `json:"currency"`
			Amount     int32  `json:"amount"`
			CVV        string `json:"cvv"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			t.Error("gateway sent invalid bank request JSON")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(request.CardNumber) < 14 || request.ExpiryDate == "" || request.Currency == "" || request.Amount <= 0 || request.CVV == "" {
			t.Error("gateway sent incomplete bank payment details")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch ending := request.CardNumber[len(request.CardNumber)-1:]; {
		case strings.Contains("13579", ending):
			_, _ = w.Write([]byte(`{"authorized":true,"authorization_code":"integration-authorization"}`))
		case strings.Contains("2468", ending):
			_, _ = w.Write([]byte(`{"authorized":false}`))
		case ending == "0":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Error("gateway forwarded a non-numeric card")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(bank.Close)
	full := []string{middleware.CardTokensCreate, middleware.PaymentIntentsCreate, middleware.PaymentIntentsConfirm, middleware.PaymentIntentsRead}
	var credentials []middleware.APIKeyCredential
	key := func(merchant string, scopes []string) string {
		payload := make([]byte, 32)
		_, err := rand.Read(payload)
		require.NoError(t, err)
		encoded := hex.EncodeToString(payload)
		hash := sha256.Sum256([]byte(encoded))
		credentials = append(credentials, middleware.APIKeyCredential{MerchantID: merchant, KeyHash: hex.EncodeToString(hash[:]), Scopes: scopes})
		return "sk_" + encoded
	}
	keys := scenarios.Credentials{
		Primary: key("integration-primary", full), Other: key("integration-other", full),
		ReadOnly: key("integration-readonly", []string{middleware.PaymentIntentsRead}),
	}
	bankClient, err := banksimulator.NewClient(bank.URL+"/", 5*time.Second)
	require.NoError(t, err)
	gateway, err := api.New(credentials, bankClient, changedatacapture.NewInMemory(),
		backgroundservices.CronJobOptions{
			SweepInterval: time.Second, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 100,
		}, 100)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(6 * time.Second):
			t.Error("integration gateway failed to stop")
		}
	})
	scenarios.Run(t, "http://"+listener.Addr().String(), keys)
}
