package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/backgroundservices"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/config"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"
)

type Api struct {
	router          *chi.Mux
	paymentIntents  *services.PaymentIntentsService
	cardTokens      *services.CardTokenService
	auth            *middleware.Authenticator
	reconciliation  *backgroundservices.AuditAndReconciliationService
	auditAndWebhook *backgroundservices.AuditAndWebhookService
	cronJobs        *backgroundservices.CronJobService
	logger          *slog.Logger
}

func New(credentials []middleware.APIKeyCredential, bank banksimulator.Client, changes changedatacapture.Streams, cronOptions backgroundservices.CronJobOptions, globalRequestsPerSecond, perMerchantRequestsPerSecond int, logger *slog.Logger) (*Api, error) {
	if logger == nil {
		return nil, errors.New("API logger is required")
	}
	auth, err := middleware.NewAuthenticator(credentials)
	if err != nil {
		return nil, fmt.Errorf("configure API key authentication: %w", err)
	}
	a := &Api{auth: auth, logger: logger}
	cardsRepo := repository.NewCardsRepository(time.Now)
	a.cardTokens = services.NewCardTokenService(cardsRepo, time.Now, logger)
	intentsRepo, err := repository.NewPaymentIntentsRepository(changes.BankTransactions, changes.PaymentIntents, time.Now)
	if err != nil {
		return nil, fmt.Errorf("initialize payment intents repository: %w", err)
	}
	a.paymentIntents, err = services.NewPaymentIntentsService(intentsRepo, bank, a.cardTokens, time.Now, logger)
	if err != nil {
		return nil, fmt.Errorf("initialize payment intents service: %w", err)
	}
	a.reconciliation, err = backgroundservices.NewAuditAndReconciliationService(changes.BankTransactions, a.logger)
	if err != nil {
		return nil, fmt.Errorf("initialize audit and reconciliation service: %w", err)
	}
	a.auditAndWebhook, err = backgroundservices.NewAuditAndWebhookService(changes.PaymentIntents, a.logger)
	if err != nil {
		return nil, fmt.Errorf("initialize audit and webhook service: %w", err)
	}
	a.cronJobs, err = backgroundservices.NewCronJobService(intentsRepo, time.Now, cronOptions)
	if err != nil {
		return nil, fmt.Errorf("initialize cron job service: %w", err)
	}
	merchantIDs := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		merchantIDs = append(merchantIDs, credential.MerchantID)
	}
	if err := a.setupRouter(merchantIDs, globalRequestsPerSecond, perMerchantRequestsPerSecond); err != nil {
		return nil, err
	}

	return a, nil
}

func (a *Api) Run(ctx context.Context, options config.ServerConfig) error {
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return fmt.Errorf("listen for HTTP requests: %w", err)
	}
	return a.Serve(ctx, listener, options)
}

// Serve runs HTTP and background workers on listener, closing it when serving stops.
func (a *Api) Serve(ctx context.Context, listener net.Listener, options config.ServerConfig) error {
	if err := options.Validate(); err != nil {
		return errors.Join(fmt.Errorf("configure HTTP server: %w", err), listener.Close())
	}
	g, ctx := errgroup.WithContext(ctx)
	httpServer := &http.Server{
		Handler:           a.router,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: options.ReadHeaderTimeout,
		ReadTimeout:       options.ReadTimeout,
		WriteTimeout:      options.WriteTimeout,
		IdleTimeout:       options.IdleTimeout,
	}

	g.Go(func() error {
		<-ctx.Done()
		a.logger.Info("Shutting down HTTP server")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			return errors.Join(fmt.Errorf("shut down HTTP server: %w", err), httpServer.Close())
		}
		return nil
	})

	g.Go(func() error {
		a.logger.Info("Starting HTTP server", "address", listener.Addr().String())
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error { return a.reconciliation.Run(ctx) })
	g.Go(func() error { return a.auditAndWebhook.Run(ctx) })
	g.Go(func() error { return a.cronJobs.Run(ctx) })

	return g.Wait()
}

func (a *Api) setupRouter(merchantIDs []string, globalRequestsPerSecond, perMerchantRequestsPerSecond int) error {
	limiter, err := middleware.NewRateLimiter(globalRequestsPerSecond)
	if err != nil {
		return err
	}
	merchantLimiter, err := middleware.NewMerchantRateLimiter(merchantIDs, perMerchantRequestsPerSecond)
	if err != nil {
		return err
	}
	metrics, err := middleware.RequestMetrics(otel.Meter("payment-gateway/http"))
	if err != nil {
		return fmt.Errorf("configure request metrics: %w", err)
	}
	a.router = chi.NewRouter()
	a.router.Use(metrics)
	a.router.Use(middleware.RequestLogging(a.logger))
	a.router.Use(middleware.Recover(a.logger))
	a.router.Use(limiter.Limit(a.logger))

	a.router.Get("/ping", a.PingHandler())
	a.router.Get("/swagger/*", a.SwaggerHandler())

	requests := a.router
	requests.With(a.auth.RequireScope(middleware.CardTokensCreate, a.logger), merchantLimiter.Limit(a.logger)).
		Post("/api/CardTokens", a.PostCardTokenHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsCreate, a.logger), merchantLimiter.Limit(a.logger), middleware.RequireIdempotencyKey(a.logger)).
		Post("/api/PaymentIntents", a.PostPaymentIntentHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsConfirm, a.logger), merchantLimiter.Limit(a.logger), middleware.RequireIdempotencyKey(a.logger)).
		Post("/api/PaymentIntents/{paymentIntentId}/confirm", a.ConfirmPaymentIntentHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsRead, a.logger), merchantLimiter.Limit(a.logger)).
		Get("/api/PaymentIntents/{paymentIntentId}", a.GetPaymentIntentHandler())
	return nil
}
