package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/requestcontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

type captureExporter struct{ records []sdklog.Record }

func (e *captureExporter) Export(_ context.Context, records []sdklog.Record) error {
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (*captureExporter) Shutdown(context.Context) error   { return nil }
func (*captureExporter) ForceFlush(context.Context) error { return nil }

func captureLogs(t *testing.T) *captureExporter {
	t.Helper()
	exporter := &captureExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	previous := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
		logglobal.SetLoggerProvider(previous)
	})
	return exporter
}

func logAttributes(record sdklog.Record) map[string]otellog.Value {
	attributes := make(map[string]otellog.Value)
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		attributes[kv.Key] = kv.Value
		return true
	})
	return attributes
}

func TestLoggingKeepsStructuredConsoleAndOTLPRecordsWithTraceCorrelation(t *testing.T) {
	exporter := captureLogs(t)
	var console bytes.Buffer
	logger, err := New(&console)
	require.NoError(t, err)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	logger = logger.With("request_id", "request", "payment_intent_id", "intent", "bank_transaction_id", "transaction")
	logger.DebugContext(ctx, "Filtered debug message")
	logger.InfoContext(ctx, `{"event_type":"banktransaction.created"}`)
	logger.WarnContext(ctx, "Manual verification required")
	require.Len(t, exporter.records, 2)
	decoder := json.NewDecoder(&console)
	for i, level := range []string{"INFO", "WARN"} {
		var record map[string]interface{}
		require.NoError(t, decoder.Decode(&record))
		assert.Equal(t, level, record["level"])
		assert.NotEmpty(t, record["@timestamp"])
		assert.NotContains(t, record, "msg")
		indexed := exporter.records[i]
		assert.Equal(t, record["message"], indexed.Body().AsString())
		assert.Equal(t, []otellog.Severity{otellog.SeverityInfo, otellog.SeverityWarn}[i], indexed.Severity())
		assert.Equal(t, spanContext.TraceID(), indexed.TraceID())
		assert.Equal(t, spanContext.SpanID(), indexed.SpanID())
		attributes := logAttributes(indexed)
		for _, key := range []string{"request_id", "payment_intent_id", "bank_transaction_id"} {
			assert.Equal(t, record[key], attributes[key].AsString())
		}
	}
}

func TestLoggerExtractsAvailableContextMetadata(t *testing.T) {
	for _, test := range []struct {
		name     string
		values   map[any]any
		expected map[string]string
	}{
		{"no metadata", nil, map[string]string{}},
		{"partial metadata", map[any]any{
			requestcontext.RequestIDKey{}:  "request",
			requestcontext.MerchantIDKey{}: "",
		}, map[string]string{"request_id": "request"}},
		{"incorrectly typed metadata", map[any]any{
			requestcontext.RequestIDKey{}: 42, requestcontext.MerchantIDKey{}: "merchant",
		}, map[string]string{"merchant_id": "merchant"}},
		{"all supported IDs", map[any]any{
			requestcontext.RequestIDKey{}:         "request",
			requestcontext.MerchantIDKey{}:        "merchant",
			requestcontext.PaymentIntentIDKey{}:   "intent",
			requestcontext.BankTransactionIDKey{}: "transaction",
			requestcontext.IdempotencyKey{}:       "01234567-89ab-4cde-8123-456789abcdef",
		}, map[string]string{
			"request_id": "request", "merchant_id": "merchant",
			"payment_intent_id": "intent", "bank_transaction_id": "transaction",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := captureLogs(t)
			var output bytes.Buffer
			logger, err := New(&output)
			require.NoError(t, err)
			spanContext := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
			})
			ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
			for key, value := range test.values {
				ctx = context.WithValue(ctx, key, value)
			}
			logger.InfoContext(ctx, "request completed")
			logger.WarnContext(ctx, "verification required")
			logger.ErrorContext(ctx, "failed to confirm payment intent", "error", errors.New("storage unavailable"))

			require.Len(t, exporter.records, 3)
			decoder := json.NewDecoder(&output)
			for i, level := range []string{"INFO", "WARN", "ERROR"} {
				var record map[string]interface{}
				require.NoError(t, decoder.Decode(&record))
				assert.Equal(t, level, record["level"])
				assert.Equal(t, []string{"request completed", "verification required", "failed to confirm payment intent"}[i], record["message"])
				indexed := exporter.records[i]
				assert.Equal(t, []otellog.Severity{otellog.SeverityInfo, otellog.SeverityWarn, otellog.SeverityError}[i], indexed.Severity())
				assert.Equal(t, record["message"], indexed.Body().AsString())
				assert.Equal(t, spanContext.TraceID(), indexed.TraceID())
				assert.Equal(t, spanContext.SpanID(), indexed.SpanID())
				attrs := logAttributes(indexed)
				if level == "ERROR" {
					assert.Equal(t, "storage unavailable", record["error"])
					assert.Equal(t, "storage unavailable", attrs["error"].AsString())
				} else {
					assert.NotContains(t, record, "error")
					assert.NotContains(t, attrs, "error")
				}
				for _, key := range []string{"request_id", "merchant_id", "idempotency_key", "payment_intent_id", "bank_transaction_id"} {
					if value, exists := test.expected[key]; exists {
						assert.Equal(t, value, record[key])
						assert.Equal(t, value, attrs[key].AsString())
					} else {
						assert.NotContains(t, record, key)
						assert.NotContains(t, attrs, key)
					}
				}
			}
		})
	}
}

func TestContextLoggerPreservesExplicitFieldsAndGroups(t *testing.T) {
	exporter := captureLogs(t)
	var output bytes.Buffer
	logger, err := New(&output)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), requestcontext.RequestIDKey{}, "context-request")
	ctx = context.WithValue(ctx, requestcontext.MerchantIDKey{}, "merchant")
	logger.With("request_id", "event-request").InfoContext(ctx, "event observed", "merchant_id", "event-merchant")
	var event map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &event))
	assert.Equal(t, "event-request", event["request_id"])
	assert.Equal(t, "event-merchant", event["merchant_id"])
	assert.Equal(t, 1, strings.Count(output.String(), `"request_id"`))
	assert.Equal(t, 1, strings.Count(output.String(), `"merchant_id"`))
	assert.Equal(t, "event-request", logAttributes(exporter.records[0])["request_id"].AsString())

	output.Reset()
	grouped := logger.WithGroup("http").With("method", "POST")
	grouped.InfoContext(ctx, "request completed", "status_code", 200)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	group, ok := record["http"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, map[string]interface{}{
		"method": "POST", "status_code": float64(200),
		"request_id": "context-request", "merchant_id": "merchant",
	}, group)
	assert.ElementsMatch(t, []otellog.KeyValue{
		otellog.String("method", "POST"), otellog.Int64("status_code", 200),
		otellog.String("request_id", "context-request"), otellog.String("merchant_id", "merchant"),
	}, logAttributes(exporter.records[1])["http"].AsMap())

	output.Reset()
	logger.InfoContext(context.Background(), "unrelated operation")
	var unrelated map[string]interface{}
	require.NoError(t, json.Unmarshal(output.Bytes(), &unrelated))
	assert.NotContains(t, unrelated, "http")
	assert.NotContains(t, unrelated, "request_id")
	assert.NotContains(t, unrelated, "merchant_id")
}

func TestLoggingPreservesGroupsAndDoesNotMutateParentLogger(t *testing.T) {
	exporter := captureLogs(t)
	var console bytes.Buffer
	logger, err := New(&console)
	require.NoError(t, err)
	logger.With("request_id", "request").WithGroup("http").With("method", "POST").Info("Request completed", "status_code", 200)
	logger.Info("Ungrouped")
	require.Len(t, exporter.records, 2)
	group := logAttributes(exporter.records[0])["http"].AsMap()
	assert.ElementsMatch(t, []otellog.KeyValue{otellog.String("method", "POST"), otellog.Int64("status_code", 200)}, group)
	assert.Empty(t, logAttributes(exporter.records[1]))
	var record map[string]interface{}
	require.NoError(t, json.NewDecoder(&console).Decode(&record))
	assert.Equal(t, "request", record["request_id"])
	assert.Equal(t, map[string]interface{}{"method": "POST", "status_code": float64(200)}, record["http"])
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLoggingSurfacesConsoleFailuresWithoutSkippingOTLP(t *testing.T) {
	exporter := captureLogs(t)
	logger, err := New(failedWriter{})
	require.NoError(t, err)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "Still exported", 0)
	assert.ErrorIs(t, logger.Handler().Handle(context.Background(), record), io.ErrClosedPipe)
	require.Len(t, exporter.records, 1)
}

func TestLoggingConsoleWorksWithoutConfiguredExport(t *testing.T) {
	previous := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(noop.NewLoggerProvider())
	defer logglobal.SetLoggerProvider(previous)
	var console bytes.Buffer
	logger, err := New(&console)
	require.NoError(t, err)
	logger.Info("Console only")
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal(console.Bytes(), &record))
	assert.Equal(t, "Console only", record["message"])
	_, err = New(nil)
	require.ErrorContains(t, err, "console log output")
}
