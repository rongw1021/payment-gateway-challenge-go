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
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"
)

type Api struct {
	router         *chi.Mux
	paymentIntents *services.PaymentIntentsService
	cardTokens     *services.CardTokenService
	auth           *middleware.Authenticator
	reconciliation *backgroundservices.ReconciliationService
	cronJobs       *backgroundservices.CronJobService
	logger         *slog.Logger
}

func New(credentials []middleware.APIKeyCredential, bank banksimulator.Client, changes changedatacapture.ChangeDataCapture, cronOptions backgroundservices.CronJobOptions, requestsPerSecond int) (*Api, error) {
	auth, err := middleware.NewAuthenticator(credentials)
	if err != nil {
		return nil, fmt.Errorf("configure API key authentication: %w", err)
	}
	a := &Api{auth: auth, logger: slog.Default()}
	cardsRepo := repository.NewCardsRepository(time.Now)
	a.cardTokens = services.NewCardTokenService(cardsRepo, time.Now)
	intentsRepo, err := repository.NewPaymentIntentsRepository(changes, time.Now)
	if err != nil {
		return nil, fmt.Errorf("initialize payment intents repository: %w", err)
	}
	a.paymentIntents, err = services.NewPaymentIntentsService(intentsRepo, bank, a.cardTokens, time.Now)
	if err != nil {
		return nil, fmt.Errorf("initialize payment intents service: %w", err)
	}
	a.reconciliation, err = backgroundservices.NewReconciliationService(changes, a.logger)
	if err != nil {
		return nil, fmt.Errorf("initialize reconciliation service: %w", err)
	}
	a.cronJobs, err = backgroundservices.NewCronJobService(intentsRepo, time.Now, cronOptions)
	if err != nil {
		return nil, fmt.Errorf("initialize cron job service: %w", err)
	}
	if err := a.setupRouter(requestsPerSecond); err != nil {
		return nil, err
	}

	return a, nil
}

func (a *Api) Run(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for HTTP requests: %w", err)
	}
	return a.Serve(ctx, listener)
}

// Serve runs HTTP and background workers on listener, closing it when serving stops.
func (a *Api) Serve(ctx context.Context, listener net.Listener) error {
	g, ctx := errgroup.WithContext(ctx)
	httpServer := &http.Server{
		Handler:     a.router,
		BaseContext: func(_ net.Listener) context.Context { return ctx },
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
	g.Go(func() error { return a.cronJobs.Run(ctx) })

	return g.Wait()
}

func (a *Api) setupRouter(requestsPerSecond int) error {
	limiter, err := middleware.NewRateLimiter(requestsPerSecond)
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
	a.router.Use(limiter.Limit)

	a.router.Get("/ping", a.PingHandler())
	a.router.Get("/swagger/*", a.SwaggerHandler())

	requests := a.router
	requests.With(a.auth.RequireScope(middleware.CardTokensCreate)).
		Post("/api/CardTokens", a.PostCardTokenHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsCreate), middleware.RequireIdempotencyKey).
		Post("/api/PaymentIntents", a.PostPaymentIntentHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsConfirm), middleware.RequireIdempotencyKey).
		Post("/api/PaymentIntents/{paymentIntentId}/confirm", a.ConfirmPaymentIntentHandler())
	requests.With(a.auth.RequireScope(middleware.PaymentIntentsRead)).
		Get("/api/PaymentIntents/{paymentIntentId}", a.GetPaymentIntentHandler())
	return nil
}
