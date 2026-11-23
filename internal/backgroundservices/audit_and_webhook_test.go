package backgroundservices

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/logging"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditAndWebhookLogsWholeEventsThenPlaceholderWarnings(t *testing.T) {
	capture := changedatacapture.NewInMemory[models.PaymentIntentChangeEvent]()
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	events := []models.PaymentIntentChangeEvent{
		{
			EventType: models.PaymentIntentCreatedEvent, RequestID: "create-request",
			MerchantID: "merchant", PaymentIntentID: "intent",
			PaymentIntent: &models.PaymentIntentSnapshot{
				Status: models.PaymentIntentCreated, Currency: "GBP", Amount: 100, CreatedAt: now, UpdatedAt: now,
			},
		},
		{
			EventType: models.PaymentIntentUpdatedEvent, RequestID: "confirm-request",
			MerchantID: "merchant", PaymentIntentID: "intent",
			Changes: []models.PaymentIntentFieldChange{{
				FieldName: "Status", OriginalValue: json.RawMessage(`"Created"`), NewValue: json.RawMessage(`"Processing"`),
			}},
		},
	}
	for _, event := range events {
		require.NoError(t, capture.Produce(event))
	}
	entries := make(logEntries, 8)
	service, err := NewAuditAndWebhookService(capture, logging.NewConsole(entries))
	require.NoError(t, err)
	cancel, done := runWorker(t, service.Run)
	logs := readWorkerLogs(t, entries, 4)
	cancel()
	require.NoError(t, <-done)
	for i, event := range events {
		for _, entry := range logs[2*i : 2*i+2] {
			assert.Equal(t, "merchant", entry["merchant_id"])
			assert.Equal(t, "intent", entry["payment_intent_id"])
			assert.Equal(t, string(event.EventType), entry["event_type"])
			assert.Equal(t, event.RequestID, entry["request_id"])
			assert.EqualValues(t, i+1, entry["sequence"])
		}
		audit, warning := logs[2*i], logs[2*i+1]
		assert.Equal(t, "INFO", audit["level"])
		var logged models.PaymentIntentChangeEvent
		require.NoError(t, json.Unmarshal([]byte(audit["message"].(string)), &logged))
		assert.Equal(t, event.WithSequence(int64(i+1)), logged)
		assert.Equal(t, "WARN", warning["level"])
		assert.Equal(t, "webhook delivery is not implemented at this time", warning["message"])
	}
	assert.Empty(t, entries)
}

type failedIntentConsumer struct{ err error }

func (f failedIntentConsumer) Produce(models.PaymentIntentChangeEvent) error { return nil }
func (f failedIntentConsumer) Consume(context.Context) (models.PaymentIntentChangeEvent, error) {
	return models.PaymentIntentChangeEvent{}, f.err
}

func TestAuditAndWebhookPropagatesErrorsAndStopsWhileIdle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, failure := range []error{errors.New("consumer failed"), io.EOF, context.Canceled} {
		service, err := NewAuditAndWebhookService(failedIntentConsumer{failure}, logger)
		require.NoError(t, err)
		assert.ErrorIs(t, service.Run(context.Background()), failure)
	}
	capture := changedatacapture.NewInMemory[models.PaymentIntentChangeEvent]()
	service, err := NewAuditAndWebhookService(capture, logger)
	require.NoError(t, err)
	waiting := make(chan struct{}, 1)
	cancel, done := runWorker(t, func(ctx context.Context) error {
		return service.Run(consumptionContext{Context: ctx, waiting: waiting})
	})
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("worker did not wait for an event")
	}
	cancel()
	require.NoError(t, <-done)
}

func TestAuditFailurePreventsWebhookWarning(t *testing.T) {
	capture := changedatacapture.NewInMemory[models.PaymentIntentChangeEvent]()
	require.NoError(t, capture.Produce(models.PaymentIntentChangeEvent{
		EventType: models.PaymentIntentUpdatedEvent,
		Changes:   []models.PaymentIntentFieldChange{{FieldName: "Status", NewValue: json.RawMessage(`{`)}},
	}))
	entries := make(logEntries, 2)
	service, err := NewAuditAndWebhookService(capture, logging.NewConsole(entries))
	require.NoError(t, err)
	require.ErrorContains(t, service.Run(context.Background()), "serialize payment intent audit event")
	assert.Empty(t, entries)
}
