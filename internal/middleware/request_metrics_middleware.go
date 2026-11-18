package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/httpresponse"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type requestMetrics struct {
	requests  metric.Int64Counter
	successes metric.Int64Counter
	duration  metric.Float64Histogram
}

func RequestMetrics(meter metric.Meter) (func(http.Handler) http.Handler, error) {
	requests, err := meter.Int64Counter("http.server.request.count", metric.WithUnit("{request}"))
	if err != nil {
		return nil, fmt.Errorf("create request counter: %w", err)
	}
	successes, err := meter.Int64Counter("http.server.request.success.count", metric.WithUnit("{request}"),
		metric.WithDescription("HTTP responses outside the 500-599 range"))
	if err != nil {
		return nil, fmt.Errorf("create HTTP success counter: %w", err)
	}
	duration, err := meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .075, .1, .25, .5, .75, 1, 2.5, 5, 7.5, 10))
	if err != nil {
		return nil, fmt.Errorf("create request duration histogram: %w", err)
	}
	m := requestMetrics{requests: requests, successes: successes, duration: duration}
	return func(next http.Handler) http.Handler {
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
				m.record(r.Context(), requestRoute(r, family), r.Method,
					httpresponse.ObservedStatus(response.Status(), completed), time.Since(started))
			}()
			next.ServeHTTP(response, r)
			completed = true
		})
	}, nil
}

func (m requestMetrics) record(ctx context.Context, route, method string, status int, duration time.Duration) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
	default:
		method = "_OTHER"
	}
	attributes := attribute.NewSet(attribute.String("http.route", route),
		attribute.String("http.request.method", method), attribute.Int("http.response.status_code", status))
	m.requests.Add(ctx, 1, metric.WithAttributeSet(attributes))
	if status < 500 || status >= 600 {
		m.successes.Add(ctx, 1, metric.WithAttributeSet(attributes))
	}
	m.duration.Record(ctx, duration.Seconds(), metric.WithAttributeSet(attributes))
}

func requestFamily(path string) string {
	for _, prefix := range []string{"/api/CardTokens", "/api/PaymentIntents"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return prefix
		}
	}
	return ""
}

func requestRoute(r *http.Request, family string) string {
	if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
		if route := routeContext.RoutePattern(); route != "" {
			return route
		}
	}
	return family + "/*"
}
