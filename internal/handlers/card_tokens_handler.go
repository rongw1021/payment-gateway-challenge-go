package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/modelbinding"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type CardTokenIssuer interface {
	Issue(context.Context, string, models.CreateCardTokenRequest) (*models.CardTokenResponse, error)
}

type CardTokensHandler struct {
	service CardTokenIssuer
	tracer  trace.Tracer
	logger  *slog.Logger
}

func NewCardTokensHandler(service CardTokenIssuer, logger *slog.Logger) *CardTokensHandler {
	return &CardTokensHandler{service: service, tracer: otel.Tracer("payment-gateway/handlers"), logger: logger}
}

// PostHandler exchanges card details for a single-use token.
//
//	@Summary		Exchange card details for a single-use payment token.
//	@Description	**Required scope:** `card-tokens:create`
//	@Description
//	@Description	### Token lifecycle
//	@Description	- Tokens are lowercase, hyphenated `UUIDv7` strings.
//	@Description	- Each token belongs to the authenticated merchant and expires exactly **15 minutes** after issuance.
//	@Description	- Supply the single-use token when confirming a payment intent.
//	@Description
//	@Description	### Card details
//	@Description	- Card numbers and CVVs must contain **ASCII digits**.
//	@Description	- The expiry month and year must not be in the past.
//	@ID				createCardToken
//	@Tags			Cards
//	@Accept			json
//	@Produce		json,application/problem+json
//	@Security		ApiKey
//	@Param			request		body		models.CreateCardTokenRequest	true	"Card details"
//	@Success		200			{object}	models.CardTokenResponse		"Single-use token and expiry timestamp"
//	@Failure		400			{object}	models.ProblemDetails			"Malformed JSON or invalid/expired card details"
//	@Failure		401,403,500	{object}	models.ProblemDetails
//	@Failure		408			{object}	models.ProblemDetails	"Request body was not received before the read deadline"
//	@Failure		413			{object}	models.ProblemDetails	"Request body exceeds 1 MiB"
//	@Failure		415			{object}	models.ProblemDetails	"Content-Type must be application/json"
//	@Failure		429			{object}	models.ProblemDetails	"Global or merchant request limit exceeded"
//	@Header			all			{string}	X-Request-Id			"Server-generated request UUIDv7 when available"
//	@Header			all			{string}	Cache-Control			"no-store"
//	@Header			all			{string}	Pragma					"no-cache"
//	@Header			401			{string}	WWW-Authenticate		"Bearer"
//	@Header			429			{integer}	Retry-After				"1 (wait one second before retrying)"
//	@Router			/api/CardTokens [post]
func (h *CardTokensHandler) PostHandler() http.HandlerFunc {
	return h.traceCardToken(func(w http.ResponseWriter, r *http.Request) {
		merchantID := middleware.MerchantIDFromContext(r.Context())
		request, ok, err := modelbinding.BindJSON[models.CreateCardTokenRequest](w, r)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
			return
		}
		if !ok {
			return
		}
		response, err := h.service.Issue(r.Context(), merchantID, *request)
		if err != nil {
			h.writeCardTokenError(r.Context(), w, err)
			return
		}
		if response == nil {
			h.writeCardTokenError(r.Context(), w, errors.New("issuance returned no card token"))
			return
		}
		h.writeCardTokenResponse(r.Context(), w, response)
	})
}

func (h *CardTokensHandler) traceCardToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := middleware.GetRequestID(r.Context())
		parent := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := h.tracer.Start(parent, "POST /api/CardTokens", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method),
				attribute.String("http.route", "/api/CardTokens"), attribute.String("request_id", requestID)))
		writer := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
		completed := false
		defer func() {
			status := httpresponse.ObservedStatus(writer.Status(), completed)
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= 500 && status < 600 {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			span.End()
		}()
		next(writer, r.WithContext(ctx))
		completed = true
	}
}

func (h *CardTokensHandler) writeCardTokenResponse(ctx context.Context, w http.ResponseWriter, response *models.CardTokenResponse) {
	response.RequestID = middleware.GetRequestID(ctx)
	if err := httpresponse.WriteJSON(w, http.StatusOK, "application/json", response); err != nil {
		h.logger.ErrorContext(ctx, "failed to write HTTP response", "error", err)
	}
}

func (h *CardTokensHandler) writeCardTokenError(ctx context.Context, w http.ResponseWriter, err error) {
	requestID := middleware.GetRequestID(ctx)
	var validation *services.ValidationError
	if errors.As(err, &validation) {
		if err := httpresponse.WriteProblem(w, http.StatusBadRequest, requestID, validation.Errors); err != nil {
			h.logger.ErrorContext(ctx, "failed to write HTTP response", "error", err)
		}
		return
	}
	h.logger.ErrorContext(ctx, "failed to issue card token", "error", err)
	if err := httpresponse.WriteProblem(w, http.StatusInternalServerError, requestID, nil); err != nil {
		h.logger.ErrorContext(ctx, "failed to write HTTP response", "error", err)
	}
}
