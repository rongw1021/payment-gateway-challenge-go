package middleware

import (
	"context"
	"log"
	"net/http"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/identifiers"
)

type requestIDKey struct{}
type idempotencyKey struct{}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := identifiers.NewUUID()
		if err != nil {
			log.Printf("Generate request ID: %v", err)
			httpresponse.WriteProblem(w, http.StatusInternalServerError, "", nil)
			return
		}
		w.Header().Set("X-Request-Id", requestID)
		ctx := WithRequestID(r.Context(), requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func GetRequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func RequireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("X-Idempotency-Key")
		if len(values) != 1 || !identifiers.IsCanonicalUUID(values[0]) {
			httpresponse.WriteProblem(w, http.StatusBadRequest, GetRequestID(r.Context()), map[string][]string{
				"idempotency_key": {"Supply exactly one idempotency key in lowercase hyphenated GUID format."},
			})
			return
		}
		ctx := context.WithValue(r.Context(), idempotencyKey{}, values[0])
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdempotencyKeyFromContext requires RequireIdempotencyKey; a missing key is a wiring error.
func IdempotencyKeyFromContext(ctx context.Context) string {
	key, ok := ctx.Value(idempotencyKey{}).(string)
	if !ok || !identifiers.IsCanonicalUUID(key) {
		panic("idempotency middleware did not provide a valid key")
	}
	return key
}

func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}
