package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Options struct {
	Enabled     bool           `yaml:"enabled"`
	ServiceName string         `yaml:"service_name"`
	OTLP        OTLPOptions    `yaml:"otlp"`
	Metrics     MetricsOptions `yaml:"metrics"`
}

type OTLPOptions struct {
	Endpoint string        `yaml:"endpoint"`
	Timeout  time.Duration `yaml:"timeout"`
}

type MetricsOptions struct {
	ExportInterval time.Duration `yaml:"export_interval"`
}

type deliveryErrors struct {
	mu    sync.Mutex
	count int
	last  error
}

func (d *deliveryErrors) Handle(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	d.last = err
}

// Configure installs process-wide providers before the API is constructed.
func Configure(ctx context.Context, options Options) (func(context.Context) error, error) {
	if override, present := os.LookupEnv("OBSERVABILITY"); present {
		enabled, err := strconv.ParseBool(override)
		if err != nil {
			return nil, errors.New("OBSERVABILITY must be a boolean (true or false)")
		}
		options.Enabled = enabled
	}
	if !options.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if strings.TrimSpace(options.ServiceName) == "" {
		return nil, errors.New("observability service_name is required")
	}
	endpoint, err := url.Parse(options.OTLP.Endpoint)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(options.OTLP.Endpoint, "#") {
		return nil, errors.New("observability otlp.endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	for _, duration := range []time.Duration{options.Metrics.ExportInterval, options.OTLP.Timeout} {
		if duration < time.Millisecond || duration > (1<<31-1)*time.Millisecond {
			return nil, errors.New("observability metrics.export_interval and otlp.timeout must be between 1 and 2147483647 milliseconds")
		}
	}
	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(options.OTLP.Endpoint), otlptracehttp.WithTimeout(options.OTLP.Timeout),
		otlptracehttp.WithURLPath(path.Join("/", endpoint.Path, "v1/traces")),
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, fmt.Errorf("configure trace exporter: %w", err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(options.OTLP.Endpoint), otlpmetrichttp.WithTimeout(options.OTLP.Timeout),
		otlpmetrichttp.WithURLPath(path.Join("/", endpoint.Path, "v1/metrics")),
		otlpmetrichttp.WithHeaders(map[string]string{}),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		otlpmetrichttp.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality {
			return metricdata.DeltaTemporality
		}))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("configure metric exporter: %w", err), traceExporter.Shutdown(ctx))
	}
	logExporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(options.OTLP.Endpoint), otlploghttp.WithTimeout(options.OTLP.Timeout),
		otlploghttp.WithURLPath(path.Join("/", endpoint.Path, "v1/logs")),
		otlploghttp.WithHeaders(map[string]string{}),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("configure log exporter: %w", err),
			traceExporter.Shutdown(ctx), metricExporter.Shutdown(ctx))
	}
	res := resource.NewSchemaless(attribute.String("service.name", options.ServiceName))
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(traceExporter, sdktrace.WithMaxQueueSize(1024), sdktrace.WithMaxExportBatchSize(256),
			sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(options.OTLP.Timeout)))
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(options.Metrics.ExportInterval), sdkmetric.WithTimeout(options.OTLP.Timeout))))
	logs := sdklog.NewLoggerProvider(sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter,
			sdklog.WithMaxQueueSize(1024), sdklog.WithExportMaxBatchSize(256),
			sdklog.WithExportInterval(time.Second), sdklog.WithExportTimeout(options.OTLP.Timeout))))
	failures := &deliveryErrors{}
	previousErrors := otel.GetErrorHandler()
	previousTraces, previousMetrics := otel.GetTracerProvider(), otel.GetMeterProvider()
	previousLogs := logglobal.GetLoggerProvider()
	previousPropagation := otel.GetTextMapPropagator()
	otel.SetErrorHandler(failures)
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(metrics)
	logglobal.SetLoggerProvider(logs)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func(ctx context.Context) error {
		err := errors.Join(traces.Shutdown(ctx), metrics.Shutdown(ctx), logs.Shutdown(ctx))
		otel.SetTracerProvider(previousTraces)
		otel.SetMeterProvider(previousMetrics)
		logglobal.SetLoggerProvider(previousLogs)
		otel.SetTextMapPropagator(previousPropagation)
		otel.SetErrorHandler(previousErrors)
		failures.mu.Lock()
		defer failures.mu.Unlock()
		if failures.count > 0 {
			return errors.Join(err, fmt.Errorf("observability reported %d errors: %w", failures.count, failures.last))
		}
		return err
	}, nil
}
