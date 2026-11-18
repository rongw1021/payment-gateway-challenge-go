package api

import (
	"encoding/json"
	"net/http"

	_ "github.com/cko-recruitment/payment-gateway-challenge-go/docs"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/handlers"
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(pong{Message: "pong"}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
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
	h := handlers.NewCardTokensHandler(a.cardTokens)
	return h.PostHandler()
}

func (a *Api) PostPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents).PostHandler()
}

func (a *Api) ConfirmPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents).ConfirmHandler()
}

func (a *Api) GetPaymentIntentHandler() http.HandlerFunc {
	return handlers.NewPaymentIntentsHandler(a.paymentIntents).GetHandler()
}
