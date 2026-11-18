package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

func RequestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		withMetadata := NoStore(RequestID(next))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			family := requestFamily(r.URL.Path)
			if family == "" {
				next.ServeHTTP(w, r)
				return
			}
			started := time.Now()
			response := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			completed := false
			defer func() {
				status := httpresponse.ObservedStatus(response.Status(), completed)
				route := requestRoute(r, family)
				logger.InfoContext(r.Context(), "HTTP request completed",
					"route", route, "method", r.Method,
					"status_code", status, "latency_ms", float64(time.Since(started))/float64(time.Millisecond),
					"request_id", response.Header().Get("X-Request-Id"))
			}()
			withMetadata.ServeHTTP(response, r)
			completed = true
		})
	}
}
