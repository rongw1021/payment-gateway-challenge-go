package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func metricReader(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality {
		return metricdata.DeltaTemporality
	}))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return provider.Meter("test"), reader
}

func collectHTTPMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	result := make(map[string]metricdata.Aggregation)
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			result[m.Name] = m.Data
		}
	}
	return result
}

func sumCounter(t *testing.T, data metricdata.Aggregation) int64 {
	t.Helper()
	if data == nil {
		return 0
	}
	sum, ok := data.(metricdata.Sum[int64])
	require.True(t, ok)
	assert.Equal(t, metricdata.DeltaTemporality, sum.Temporality)
	var total int64
	for _, point := range sum.DataPoints {
		total += point.Value
	}
	return total
}

func TestRequestMetricsCountsNon5xxSuccessesAndRecordsSafeLatencyDistributions(t *testing.T) {
	meter, reader := metricReader(t)
	instrument, err := RequestMetrics(meter)
	require.NoError(t, err)
	router := chi.NewRouter()
	router.Use(instrument)
	status := http.StatusOK
	router.Get("/api/PaymentIntents/{paymentIntentId}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		w.WriteHeader(status)
	})
	for _, code := range []int{200, 302, 400, 401, 403, 404, 409, 500, 503, 599} {
		status = code
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
			"/api/PaymentIntents/private-id?card_token=private-token", nil))
	}
	for _, path := range []string{"/ping", "/swagger/index.html", "/api/PaymentIntents-other"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	data := collectHTTPMetrics(t, reader)
	assert.EqualValues(t, 10, sumCounter(t, data["http.server.request.count"]))
	assert.EqualValues(t, 7, sumCounter(t, data["http.server.request.success.count"]))
	histogram, ok := data["http.server.request.duration"].(metricdata.Histogram[float64])
	require.True(t, ok)
	var samples uint64
	for _, point := range histogram.DataPoints {
		samples += point.Count
		assert.Greater(t, point.Sum, float64(0))
		route, exists := point.Attributes.Value("http.route")
		require.True(t, exists)
		assert.Equal(t, "/api/PaymentIntents/{paymentIntentId}", route.AsString())
		assert.Equal(t, 3, point.Attributes.Len())
		assert.NotContains(t, point.Attributes.Encoded(attribute.DefaultEncoder()), "private-id")
	}
	assert.EqualValues(t, 10, samples)
	next := collectHTTPMetrics(t, reader)
	assert.Zero(t, sumCounter(t, next["http.server.request.count"]), "delta exports must not recount prior requests")
}

func TestRequestMetricsRecordUnmatchedRequestsAndPanics(t *testing.T) {
	meter, reader := metricReader(t)
	instrument, err := RequestMetrics(meter)
	require.NoError(t, err)
	router := chi.NewRouter()
	router.Use(instrument)
	router.Post("/api/CardTokens", func(http.ResponseWriter, *http.Request) { panic("test panic") })
	assert.PanicsWithValue(t, "test panic", func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/CardTokens", nil))
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("CUSTOM-METHOD", "/api/PaymentIntents/private-path", nil))
	data := collectHTTPMetrics(t, reader)
	assert.EqualValues(t, 2, sumCounter(t, data["http.server.request.count"]))
	assert.EqualValues(t, 1, sumCounter(t, data["http.server.request.success.count"]))
	sum := data["http.server.request.count"].(metricdata.Sum[int64])
	for _, point := range sum.DataPoints {
		status, _ := point.Attributes.Value("http.response.status_code")
		method, _ := point.Attributes.Value("http.request.method")
		route, _ := point.Attributes.Value("http.route")
		if status.AsInt64() == 500 {
			assert.Equal(t, "/api/CardTokens", route.AsString())
		} else {
			assert.Equal(t, "_OTHER", method.AsString())
			assert.Equal(t, "/api/PaymentIntents/*", route.AsString())
		}
	}
}

func TestRequestMetricSamplesSupportQPSRateAndP95WithoutAveragingPercentiles(t *testing.T) {
	meter, reader := metricReader(t)
	requests, err := meter.Int64Counter("http.server.request.count")
	require.NoError(t, err)
	successes, err := meter.Int64Counter("http.server.request.success.count")
	require.NoError(t, err)
	duration, err := meter.Float64Histogram("http.server.request.duration",
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, 1))
	require.NoError(t, err)
	recorder := requestMetrics{requests: requests, successes: successes, duration: duration}
	for i := 0; i < 20; i++ {
		status, elapsed := 400, time.Millisecond
		if i >= 16 {
			status = 503
		}
		if i == 19 {
			elapsed = 200 * time.Millisecond
		}
		recorder.record(context.Background(), "/api/CardTokens", http.MethodPost, status, elapsed)
	}
	data := collectHTTPMetrics(t, reader)
	count := sumCounter(t, data["http.server.request.count"])
	success := sumCounter(t, data["http.server.request.success.count"])
	assert.Equal(t, float64(2), float64(count)/10, "20 requests over 10 seconds")
	assert.Equal(t, float64(80), float64(success)/float64(count)*100)
	histogram := data["http.server.request.duration"].(metricdata.Histogram[float64])
	var countBelowFiveMilliseconds, total uint64
	for _, point := range histogram.DataPoints {
		countBelowFiveMilliseconds += point.BucketCounts[0]
		total += point.Count
	}
	assert.EqualValues(t, 20, total)
	assert.EqualValues(t, 19, countBelowFiveMilliseconds, "the merged histogram retains 95% of samples below 5ms despite the outlier")
}
