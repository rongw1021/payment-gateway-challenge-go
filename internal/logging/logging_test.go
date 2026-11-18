package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

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
	logger.InfoContext(ctx, `{"event_type":"Created"}`)
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
