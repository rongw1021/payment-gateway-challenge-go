package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/docs"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/api"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/config"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/telemetry"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// @title						Payment Gateway Challenge Go
// @version					dev
// @description				Merchant-scoped card tokenization and payment intents with **idempotent confirmation** and bank transaction history.
// @description
// @description				### Authentication
// @description				- Send `Authorization: Bearer sk_<payload>` on card-token and payment-intent requests.
// @description				- Use an **API key** with the required endpoint scope.
// @description
// @description				### Requests and responses
// @description				- POST bodies must be single JSON objects no larger than **1 MiB**.
// @description				- Success responses use `application/json`; errors use `application/problem+json`.
// @description				- Payment endpoints return a fresh `X-Request-Id` and disable response caching.
// @description
// @description				### HTTP deadlines
// @description				- Configurable server defaults: **5s** for headers, **15s** for headers and body, **30s** for response writes, and **60s** for idle keep-alive connections.
// @description				- Slow POST body uploads return **408 Request Timeout** before payment processing. Incomplete headers or stalled response readers may result in a closed connection without a JSON response.
// @description				- The write deadline includes body reading and handler time. It must exceed the request-read budget plus the bank timeout; it does not cancel bank processing or prove that a payment failed.
// @description
// @description				### Rate limiting
// @description				- A configurable global rolling limit defaults to **100 requests per second** per process.
// @description				- `GET /ping` is exempt.
// @description				- Excess requests return **429 Too Many Requests** with `Retry-After: 1`.
// @BasePath					/
// @securityDefinitions.apikey	ApiKey
// @in							header
// @name						Authorization
// @description				Enter the full `Bearer sk_<payload>` value. This is an **API key**, not JWT or HTTP Basic authentication.
func main() {
	slog.SetDefault(logging.NewConsole(os.Stdout))
	docs.SwaggerInfo.Version = version

	err := run()
	if err != nil {
		slog.Error("Fatal API error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	console := slog.Default()
	// SDK diagnostics must not be fed back into their own OTLP log exporter.
	otel.SetLogger(logr.FromSlogHandler(console.Handler()))
	shutdown, err := telemetry.Configure(context.Background(), cfg.Observability)
	if err != nil {
		return fmt.Errorf("configure observability: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := shutdown(ctx)
		slog.SetDefault(console)
		if err != nil {
			console.Warn("Observability delivery incomplete at shutdown", "error", err)
		}
	}()
	logger, err := logging.New(os.Stdout)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	slog.SetDefault(logger)
	logger.Info("Starting payment gateway", "version", version, "commit", commit, "build_time", date)
	bank, err := banksimulator.NewClient(cfg.BankSimulator.BaseAddress, cfg.BankSimulator.Timeout)
	if err != nil {
		return fmt.Errorf("configure bank simulator: %w", err)
	}
	cronOptions := backgroundservices.CronJobOptions{
		SweepInterval:                      cfg.Reconciliation.SweepInterval,
		BankTransactionStatusUpdateTimeout: cfg.Reconciliation.BankTransactionStatusUpdateTimeout,
		BatchSize:                          int(cfg.Reconciliation.BatchSize),
	}
	api, err := api.New(cfg.Authentication.APIKeys, bank, changedatacapture.NewInMemory(), cronOptions,
		int(cfg.RateLimit.RequestsPerSecond))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	defer func() {
		// recover after panic
		if x := recover(); x != nil {
			logger.Error("Runtime panic", "panic_type", fmt.Sprintf("%T", x))
			panic(x)
		}
	}()

	if err := api.Run(ctx, cfg.Server); err != nil {
		return err
	}

	return nil
}
