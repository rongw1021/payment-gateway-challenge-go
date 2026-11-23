package api

import (
	"net/http"

	_ "github.com/cko-recruitment/payment-gateway-challenge-go/docs"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/handlers"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	httpSwagger "github.com/swaggo/http-swagger"
)

type pong struct {
	Message string `json:"message" binding:"required" example:"pong"`
}

// PingHandler returns an http.HandlerFunc that handles HTTP Ping GET requests.
//
//	@Summary		Check gateway liveness.
//	@Description	- **Authentication:** Public endpoint; no API key is required.
//	@Description	- **Rate limit:** Exempt from the global rate limit.
//	@ID				ping
//	@Tags			Health
//	@Produce		json
//	@Success		200	{object}	pong
//	@Router			/ping [get]
func (a *Api) PingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := httpresponse.WriteJSON(w, http.StatusOK, "application/json", pong{Message: "pong"}); err != nil {
			a.logger.ErrorContext(r.Context(), "failed to write HTTP response", "error", err)
		}
	}
}

// SwaggerHandler returns an http.HandlerFunc that handles HTTP Swagger related requests.
func (a *Api) SwaggerHandler() http.HandlerFunc {
	return httpSwagger.Handler(
		httpSwagger.URL("/swagger/doc.json"),
	)
}

func (a *Api) PostCardTokenHandler() http.HandlerFunc {
	h := handlers.NewCardTokensHandler(a.cardTokens, a.logger)
	return h.PostHandler()
}

func (a *Api) PostPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents, a.logger).PostHandler()
}

func (a *Api) ConfirmPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents, a.logger).ConfirmHandler()
}

func (a *Api) GetPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents, a.logger).GetHandler()
}
