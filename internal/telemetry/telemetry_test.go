package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func withoutObservabilityOverride(t *testing.T) {
	t.Helper()
	t.Setenv("OBSERVABILITY", "")
	require.NoError(t, os.Unsetenv("OBSERVABILITY"))
}

func testTelemetry(t *testing.T, endpoint string) func(context.Context) error {
	t.Helper()
	withoutObservabilityOverride(t)
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	logglobal.SetLoggerProvider(lognoop.NewLoggerProvider())
	stop, err := Configure(context.Background(), Options{
		Enabled: true, ServiceName: "gateway-test",
		OTLP:    OTLPOptions{Endpoint: endpoint, Timeout: time.Second},
		Metrics: MetricsOptions{ExportInterval: time.Hour},
	})
	require.NoError(t, err)
	return cleanupTelemetry(t, stop)
}

func cleanupTelemetry(t *testing.T, stop func(context.Context) error) func(context.Context) error {
	t.Helper()
	var once sync.Once
	var stopErr error
	shutdown := func(ctx context.Context) error {
		once.Do(func() { stopErr = stop(ctx) })
		return stopErr
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// Tests assert expected delivery failures; cleanup must still stop all providers.
		_ = shutdown(ctx)
	})
	return shutdown
}

func TestConfigureExportsLogsDeltaMetricsAndTracesOverOTLP(t *testing.T) {
	logs := make(chan *collectorlog.ExportLogsServiceRequest, 2)
	metrics := make(chan *collectormetrics.ExportMetricsServiceRequest, 2)
	traces := make(chan *collectortrace.ExportTraceServiceRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
		assert.Empty(t, r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		switch r.URL.Path {
		case "/collector/v1/logs":
			request := &collectorlog.ExportLogsServiceRequest{}
			assert.NoError(t, proto.Unmarshal(body, request))
			logs <- request
		case "/collector/v1/metrics":
			request := &collectormetrics.ExportMetricsServiceRequest{}
			assert.NoError(t, proto.Unmarshal(body, request))
			metrics <- request
		case "/collector/v1/traces":
			request := &collectortrace.ExportTraceServiceRequest{}
			assert.NoError(t, proto.Unmarshal(body, request))
			traces <- request
		default:
			t.Errorf("unexpected OTLP path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	stop := testTelemetry(t, server.URL+"/collector/")
	counter, err := otel.Meter("test").Int64Counter("http.server.request.count")
	require.NoError(t, err)
	counter.Add(context.Background(), 3, metric.WithAttributes(attribute.String("http.route", "/api/CardTokens")))
	spanCtx, span := otel.Tracer("test").Start(context.Background(), "POST /api/CardTokens")
	logger, err := logging.New(io.Discard)
	require.NoError(t, err)
	logger.InfoContext(spanCtx, "Request completed", "request_id", "request", "status_code", 200)
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, stop(ctx))
	select {
	case data := <-logs:
		require.Len(t, data.ResourceLogs, 1)
		resource := data.ResourceLogs[0]
		assert.Equal(t, "gateway-test", resource.Resource.Attributes[0].Value.GetStringValue())
		records := resource.ScopeLogs[0].LogRecords
		require.Len(t, records, 1)
		assert.Equal(t, "Request completed", records[0].Body.GetStringValue())
		assert.Equal(t, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, records[0].SeverityNumber)
		traceID := span.SpanContext().TraceID()
		assert.Equal(t, traceID[:], records[0].TraceId)
	case <-ctx.Done():
		t.Fatal("logs not exported")
	}
	select {
	case data := <-metrics:
		require.Len(t, data.ResourceMetrics, 1)
		resource := data.ResourceMetrics[0]
		assert.Equal(t, "gateway-test", resource.Resource.Attributes[0].Value.GetStringValue())
		sum := resource.ScopeMetrics[0].Metrics[0].GetSum()
		require.NotNil(t, sum)
		assert.Equal(t, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, sum.AggregationTemporality)
		require.Len(t, sum.DataPoints, 1)
		assert.EqualValues(t, 3, sum.DataPoints[0].GetAsInt())
	case <-ctx.Done():
		t.Fatal("metrics not exported")
	}
	select {
	case data := <-traces:
		require.Len(t, data.ResourceSpans, 1)
		spans := data.ResourceSpans[0].ScopeSpans[0].Spans
		require.Len(t, spans, 1)
		assert.Equal(t, "POST /api/CardTokens", spans[0].Name)
		assert.Len(t, spans[0].TraceId, 16)
		assert.GreaterOrEqual(t, spans[0].EndTimeUnixNano, spans[0].StartTimeUnixNano)
	case <-ctx.Done():
		t.Fatal("trace not exported")
	}
}

func TestConfigureReportsExportFailureAtShutdownWithoutBlockingSpanCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	stop := testTelemetry(t, server.URL)
	_, span := otel.Tracer("test").Start(context.Background(), "POST /api/CardTokens")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, stop(ctx))
}

func TestConfigureKeepsSlowExportOffRequestPathAndBoundsShutdown(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	stop := testTelemetry(t, server.URL)
	returned := make(chan struct{})
	go func() {
		_, span := otel.Tracer("test").Start(context.Background(), "POST /api/CardTokens")
		span.End()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("span creation waited for the unavailable collector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	before := time.Now()
	err := stop(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(before), 500*time.Millisecond)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not attempt the queued export")
	}
}

func TestConfigureResumesAllSignalsWhenCollectorBecomesAvailable(t *testing.T) {
	var available atomic.Bool
	failed := make(chan struct{}, 1)
	received := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !available.Load() {
			failed <- struct{}{}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		received <- r.URL.Path
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	stop := testTelemetry(t, server.URL)
	var console bytes.Buffer
	logger, err := logging.New(&console)
	require.NoError(t, err)
	logger.Info("Before Collector is available")
	provider, ok := logglobal.GetLoggerProvider().(*sdklog.LoggerProvider)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, provider.ForceFlush(ctx))
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("export was not attempted while Collector was unavailable")
	}
	available.Store(true)
	logger.Info("After Collector is available")
	counter, err := otel.Meter("test").Int64Counter("http.server.request.count")
	require.NoError(t, err)
	counter.Add(ctx, 1)
	_, span := otel.Tracer("test").Start(ctx, "POST /api/CardTokens")
	span.End()
	require.ErrorContains(t, stop(ctx), "observability reported")
	paths := make(map[string]bool)
	for len(paths) < 3 {
		select {
		case path := <-received:
			paths[path] = true
		case <-ctx.Done():
			t.Fatal("all signals did not resume after Collector recovery")
		}
	}
	assert.True(t, paths["/v1/logs"])
	assert.True(t, paths["/v1/metrics"])
	assert.True(t, paths["/v1/traces"])
	assert.Contains(t, console.String(), "Before Collector is available")
	assert.Contains(t, console.String(), "After Collector is available")
}

func TestConfigureDisabledAndInvalidOptions(t *testing.T) {
	withoutObservabilityOverride(t)
	stop, err := Configure(context.Background(), Options{})
	require.NoError(t, err)
	require.NoError(t, stop(context.Background()))
	for _, edit := range []func(*Options){
		func(o *Options) { o.ServiceName = " " },
		func(o *Options) { o.OTLP.Endpoint = "relative" },
		func(o *Options) { o.OTLP.Endpoint = "http://user:secret@localhost:4318" },
		func(o *Options) { o.OTLP.Endpoint = "http://localhost:4318/?secret=value" },
		func(o *Options) { o.Metrics.ExportInterval = 0 },
		func(o *Options) { o.OTLP.Timeout = -time.Second },
	} {
		options := Options{Enabled: true, ServiceName: "gateway",
			OTLP:    OTLPOptions{Endpoint: "http://localhost:4318", Timeout: time.Second},
			Metrics: MetricsOptions{ExportInterval: time.Second}}
		edit(&options)
		_, err := Configure(context.Background(), options)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
}

func TestConfigureObservabilityEnvironmentOverridesYAML(t *testing.T) {
	for _, test := range []struct {
		name       string
		override   string
		present    bool
		configured bool
		wantExport bool
	}{
		{"unset preserves disabled", "", false, false, false},
		{"unset preserves enabled", "", false, true, true},
		{"true enables disabled YAML", "true", true, false, true},
		{"false disables enabled YAML", "false", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			withoutObservabilityOverride(t)
			if test.present {
				t.Setenv("OBSERVABILITY", test.override)
			}
			var exports, traceExports atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Contains(t, []string{"/v1/traces", "/v1/metrics", "/v1/logs"}, r.URL.Path)
				_, _ = io.Copy(io.Discard, r.Body)
				exports.Add(1)
				if r.URL.Path == "/v1/traces" {
					traceExports.Add(1)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			t.Cleanup(server.Close)
			previous := otel.GetTracerProvider()
			otel.SetTracerProvider(tracenoop.NewTracerProvider())
			t.Cleanup(func() { otel.SetTracerProvider(previous) })
			stop, err := Configure(context.Background(), Options{
				Enabled: test.configured, ServiceName: "override-test",
				OTLP:    OTLPOptions{Endpoint: server.URL, Timeout: time.Second},
				Metrics: MetricsOptions{ExportInterval: time.Hour},
			})
			require.NoError(t, err)
			shutdown := cleanupTelemetry(t, stop)
			_, span := otel.Tracer("test").Start(context.Background(), "override-test")
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			require.NoError(t, shutdown(ctx))
			if test.wantExport {
				assert.EqualValues(t, 1, traceExports.Load())
			} else {
				assert.Zero(t, exports.Load())
			}
		})
	}
}

func TestConfigureRejectsInvalidObservabilityOverride(t *testing.T) {
	for _, value := range []string{"", " ", "enabled", "private-value"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OBSERVABILITY", value)
			_, err := Configure(context.Background(), Options{})
			require.ErrorContains(t, err, "OBSERVABILITY")
			assert.NotContains(t, err.Error(), "private-value")
		})
	}
	t.Setenv("OBSERVABILITY", "true")
	_, err := Configure(context.Background(), Options{})
	require.ErrorContains(t, err, "service_name", "enabling export still requires valid YAML settings")
}
