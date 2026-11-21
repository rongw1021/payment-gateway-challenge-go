package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/modelbinding"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/services"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type PaymentIntentService interface {
	CreatePaymentIntent(context.Context, string, string, models.CreatePaymentIntentRequest) (*models.PaymentIntentResponse, bool, error)
	ConfirmPaymentIntent(context.Context, string, string, string, models.ConfirmPaymentIntentRequest) (*models.PaymentIntentResponse, error)
	GetPaymentIntent(string, string) (*models.PaymentIntentResponse, error)
}

type PaymentIntentsHandler struct {
	service PaymentIntentService
}

func NewPaymentIntentsHandler(service PaymentIntentService) *PaymentIntentsHandler {
	return &PaymentIntentsHandler{service: service}
}

// PostHandler creates a payment intent.
//
//	@Summary		Create a payment intent without submitting a payment to the bank.
//	@Description	**Required scope:** `payment-intents:create`
//	@Description
//	@Description	### Creation
//	@Description	- Record the payment intent without contacting the bank.
//	@Description	- Return **201 Created** with the current intent, transaction history, and a `Location` lookup URL.
//	@Description
//	@Description	### Idempotent replay
//	@Description	- The same merchant, idempotency key, currency, and amount return the current intent and history with **201 Created** and the same `Location`.
//	@Description	- A changed amount or currency under the same key returns **409 Conflict**.
//	@Description	- Creation and confirmation use **separate idempotency-key namespaces**.
//	@ID				createPaymentIntent
//	@Tags			Payment intents
//	@Accept			json
//	@Produce		json,application/problem+json
//	@Security		ApiKey
//	@Param			X-Idempotency-Key	header		string								true	"Lowercase hyphenated GUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)"	minlength(36)	maxlength(36)	format(uuid)
//	@Param			request				body		models.CreatePaymentIntentRequest	true	"Intent details"
//	@Success		201					{object}	models.PaymentIntentResponse		"Created intent, or current state on an idempotent replay"
//	@Failure		400					{object}	models.ProblemDetails				"Invalid JSON, intent details, or idempotency key"
//	@Failure		401,403,500			{object}	models.ProblemDetails
//	@Failure		408					{object}	models.ProblemDetails	"Request body was not received before the read deadline"
//	@Failure		409					{object}	models.ProblemDetails	"Idempotency key reused with different intent details"
//	@Failure		413					{object}	models.ProblemDetails	"Request body exceeds 1 MiB"
//	@Failure		415					{object}	models.ProblemDetails	"Content-Type must be application/json"
//	@Failure		429					{object}	models.ProblemDetails	"Global request limit exceeded"
//	@Header			201					{string}	Location				"Relative payment-intent lookup URL, also returned on replay"
//	@Header			all					{string}	X-Request-Id			"Server-generated request UUIDv7 when available"
//	@Header			all					{string}	Cache-Control			"no-store"
//	@Header			all					{string}	Pragma					"no-cache"
//	@Header			401					{string}	WWW-Authenticate		"Bearer"
//	@Header			429					{integer}	Retry-After				"1 (wait one second before retrying)"
//	@Router			/api/PaymentIntents [post]
func (h *PaymentIntentsHandler) PostHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := middleware.MerchantIDFromContext(r.Context())
		idempotencyKey := middleware.IdempotencyKeyFromContext(r.Context())
		request, ok := modelbinding.BindJSON[models.CreatePaymentIntentRequest](w, r)
		if !ok {
			return
		}
		intent, conflict, err := h.service.CreatePaymentIntent(r.Context(), merchantID, idempotencyKey, *request)
		if err != nil {
			writePaymentIntentError(r.Context(), w, err)
			return
		}
		if conflict {
			writePaymentIntentProblem(r.Context(), w, http.StatusConflict,
				"The idempotency key was already used with different intent details.")
			return
		}
		if intent == nil {
			writePaymentIntentError(r.Context(), w, errors.New("creation returned no payment intent"))
			return
		}
		w.Header().Set("Location", "/api/PaymentIntents/"+intent.PaymentIntentID)
		writePaymentIntentResponse(r.Context(), w, http.StatusCreated, intent)
	}
}

// ConfirmHandler confirms a payment intent.
//
//	@Summary		Confirm a payment intent using a single-use card token.
//	@Description	**Required scope:** `payment-intents:confirm`
//	@Description
//	@Description	### New attempts
//	@Description	- Allowed only when the intent is `Created` or `Declined`.
//	@Description	- Consume a merchant-owned, single-use card token.
//	@Description
//	@Description	### Idempotent replay
//	@Description	- Reuse the same idempotency key, intent, and card token, even after token expiry.
//	@Description	- Matching replays reuse the attempt and follow the **current intent state**, not a cached original response.
//	@Description
//	@Description	### Response states
//	@Description	- **200 OK:** `Authorized` or `Declined`, with the complete transaction history.
//	@Description	- **202 Accepted:** `PendingVerification`, with complete history and a `Location` lookup URL.
//	@Description	- **409 Conflict:** The attempt is still `Processing`, the intent cannot start another attempt, or the idempotency key conflicts.
//	@Description
//	@Description	> **Pending verification:** A `202` response is not an authorization or a decline and does not guarantee eventual verification or success.
//	@Description	> **Do not retry with a new idempotency key.** Use the `Location` URL to retrieve the current intent.
//	@ID				confirmPaymentIntent
//	@Tags			Payment intents
//	@Accept			json
//	@Produce		json,application/problem+json
//	@Security		ApiKey
//	@Param			paymentIntentId		path		string								true	"Payment intent UUID"												format(uuid)
//	@Param			X-Idempotency-Key	header		string								true	"Lowercase hyphenated GUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)"	minlength(36)	maxlength(36)	format(uuid)
//	@Param			request				body		models.ConfirmPaymentIntentRequest	true	"Single-use card token"
//	@Success		200					{object}	models.PaymentIntentResponse		"Authorized or Declined intent with its complete transaction history"
//	@Success		202					{object}	models.PaymentIntentResponse		"Accepted confirmation with an unconfirmed bank outcome and complete transaction history"
//	@Failure		400					{object}	models.ProblemDetails				"Invalid request/key or invalid, expired, consumed, or foreign card token"
//	@Failure		401,403,500			{object}	models.ProblemDetails
//	@Failure		408					{object}	models.ProblemDetails	"Request body was not received before the read deadline"
//	@Failure		404					{object}	models.ProblemDetails	"Intent ID is malformed, missing, or owned by another merchant"
//	@Failure		409					{object}	models.ProblemDetails	"Idempotency conflict, intent cannot be paid, or attempt is still Processing"
//	@Failure		413					{object}	models.ProblemDetails	"Request body exceeds 1 MiB"
//	@Failure		415					{object}	models.ProblemDetails	"Content-Type must be application/json"
//	@Failure		429					{object}	models.ProblemDetails	"Global request limit exceeded"
//	@Header			202					{string}	Location				"Relative payment-intent lookup URL, also returned on pending-verification replay"
//	@Header			all					{string}	X-Request-Id			"Server-generated request UUIDv7 when available"
//	@Header			all					{string}	Cache-Control			"no-store"
//	@Header			all					{string}	Pragma					"no-cache"
//	@Header			401					{string}	WWW-Authenticate		"Bearer"
//	@Header			429					{integer}	Retry-After				"1 (wait one second before retrying)"
//	@Router			/api/PaymentIntents/{paymentIntentId}/confirm [post]
func (h *PaymentIntentsHandler) ConfirmHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := middleware.MerchantIDFromContext(r.Context())
		idempotencyKey := middleware.IdempotencyKeyFromContext(r.Context())
		id, ok := paymentIntentID(w, r)
		if !ok {
			return
		}
		request, ok := modelbinding.BindJSON[models.ConfirmPaymentIntentRequest](w, r)
		if !ok {
			return
		}
		intent, err := h.service.ConfirmPaymentIntent(r.Context(), merchantID, id, idempotencyKey, *request)
		if err != nil {
			writePaymentIntentError(r.Context(), w, err)
			return
		}
		if intent == nil {
			writePaymentIntentError(r.Context(), w, errors.New("confirmation returned no payment intent"))
			return
		}
		switch intent.Status {
		case models.PaymentIntentAuthorized, models.PaymentIntentDeclined:
			writePaymentIntentResponse(r.Context(), w, http.StatusOK, intent)
		case models.PaymentIntentPendingVerification:
			w.Header().Set("Location", "/api/PaymentIntents/"+intent.PaymentIntentID)
			writePaymentIntentResponse(r.Context(), w, http.StatusAccepted, intent)
		case models.PaymentIntentProcessing:
			writeAttemptProblem(r.Context(), w, http.StatusConflict,
				"The bank transaction is already being processed.", intent)
		default:
			writePaymentIntentError(r.Context(), w, errors.New("unsupported payment intent status"))
		}
	}
}

// GetHandler retrieves a merchant-owned payment intent.
//
//	@Summary		Retrieve the current state of a merchant-owned payment intent.
//	@Description	**Required scope:** `payment-intents:read`
//	@Description
//	@Description	### Response
//	@Description	- Return **200 OK** with the merchant-owned intent's current state and complete transaction history, **oldest first**.
//	@Description	- A new intent has an empty transaction history.
//	@Description	- Malformed, missing, or foreign intent IDs return **404 Not Found**.
//	@ID				getPaymentIntent
//	@Tags			Payment intents
//	@Produce		json,application/problem+json
//	@Security		ApiKey
//	@Param			paymentIntentId	path		string	true	"Payment intent UUID"	format(uuid)
//	@Success		200				{object}	models.PaymentIntentResponse
//	@Failure		401,403,500		{object}	models.ProblemDetails
//	@Failure		404				{object}	models.ProblemDetails	"Intent ID is malformed, missing, or owned by another merchant"
//	@Failure		429				{object}	models.ProblemDetails	"Global request limit exceeded"
//	@Header			all				{string}	X-Request-Id			"Server-generated request UUIDv7 when available"
//	@Header			all				{string}	Cache-Control			"no-store"
//	@Header			all				{string}	Pragma					"no-cache"
//	@Header			401				{string}	WWW-Authenticate		"Bearer"
//	@Header			429				{integer}	Retry-After				"1 (wait one second before retrying)"
//	@Router			/api/PaymentIntents/{paymentIntentId} [get]
func (h *PaymentIntentsHandler) GetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := middleware.MerchantIDFromContext(r.Context())
		if err := r.Context().Err(); err != nil {
			writePaymentIntentError(r.Context(), w, err)
			return
		}
		id, ok := paymentIntentID(w, r)
		if !ok {
			return
		}
		intent, err := h.service.GetPaymentIntent(merchantID, id)
		if err != nil {
			writePaymentIntentError(r.Context(), w, err)
			return
		}
		if intent == nil {
			writePaymentIntentError(r.Context(), w, models.ErrPaymentIntentNotFound)
			return
		}
		writePaymentIntentResponse(r.Context(), w, http.StatusOK, intent)
	}
}

func paymentIntentID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "paymentIntentId"))
	if err != nil {
		writePaymentIntentError(r.Context(), w, models.ErrPaymentIntentNotFound)
		return "", false
	}
	return id.String(), true
}

func writePaymentIntentResponse(ctx context.Context, w http.ResponseWriter, status int, intent *models.PaymentIntentResponse) {
	requestID := middleware.GetRequestID(ctx)
	intent.RequestID = &requestID
	httpresponse.WriteJSON(w, status, "application/json", intent)
}

func writePaymentIntentProblem(ctx context.Context, w http.ResponseWriter, status int, detail string) {
	httpresponse.WriteProblemDetails(w, models.ProblemDetails{
		Status: status, Detail: detail, RequestID: middleware.GetRequestID(ctx),
	})
}

func writePaymentIntentError(ctx context.Context, w http.ResponseWriter, err error) {
	requestID := middleware.GetRequestID(ctx)
	var validation *services.ValidationError
	if errors.As(err, &validation) {
		httpresponse.WriteProblem(w, http.StatusBadRequest, requestID, validation.Errors)
		return
	}
	status, detail := http.StatusInternalServerError, ""
	switch {
	case errors.Is(err, models.ErrPaymentIntentNotFound):
		status, detail = http.StatusNotFound, "Payment intent not found."
	case errors.Is(err, models.ErrIdempotencyConflict):
		status, detail = http.StatusConflict, "The idempotency key was already used with a different intent or token."
	case errors.Is(err, models.ErrIntentNotPayable):
		status, detail = http.StatusConflict, "The intent is already authorized, processing, or awaiting verification."
	case errors.Is(err, models.ErrInvalidCardToken):
		status, detail = http.StatusBadRequest, "The card token is invalid or unavailable."
	default:
		log.Printf("Payment intent request failed (request_id=%s): %v", requestID, err)
	}
	writePaymentIntentProblem(ctx, w, status, detail)
}

func writeAttemptProblem(ctx context.Context, w http.ResponseWriter, status int, detail string, intent *models.PaymentIntentResponse) {
	for _, transaction := range intent.BankTransactions {
		intentStatus, err := transaction.Status.ToPaymentIntentStatus()
		if err != nil {
			writePaymentIntentError(ctx, w, err)
			return
		}
		if intentStatus != intent.Status {
			continue
		}
		httpresponse.WriteProblemDetails(w, models.ProblemDetails{
			Status: status, Detail: detail, RequestID: middleware.GetRequestID(ctx),
			PaymentIntentID: intent.PaymentIntentID, BankTransactionID: transaction.BankTransactionID,
			PaymentIntentStatus: intent.Status, BankTransactionStatus: transaction.Status,
		})
		return
	}
	writePaymentIntentError(ctx, w, errors.New("payment intent has no matching active bank transaction"))
}
