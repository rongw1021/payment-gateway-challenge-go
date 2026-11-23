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
	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type logEntries chan map[string]interface{}

func (entries logEntries) Write(data []byte) (int, error) {
	var entry map[string]interface{}
	if err := json.Unmarshal(data, &entry); err != nil {
		return 0, err
	}
	entries <- entry
	return len(data), nil
}

func readWorkerLogs(t *testing.T, entries logEntries, count int) []map[string]interface{} {
	t.Helper()
	result := make([]map[string]interface{}, 0, count)
	for len(result) < count {
		select {
		case entry := <-entries:
			result = append(result, entry)
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not log all events")
		}
	}
	return result
}

func TestReconciliationLogsEveryChangeAndWarnsOnlyOnVerificationTransitions(t *testing.T) {
	for _, failure := range []models.BankTransactionStatus{
		models.BankTransactionRequestTimeout, models.BankTransactionStatusUpdateTimeout,
		models.BankTransactionServiceUnavailable, models.BankTransactionNetworkError,
		models.BankTransactionInvalidResponse, models.BankTransactionRequestCanceled,
	} {
		t.Run(string(failure), func(t *testing.T) {
			repo, capture, clock := workerRepository(t)
			transaction := workerTransaction(t, repo)
			_, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, failure, nil)
			require.NoError(t, err)
			require.NoError(t, capture.Produce(models.BankTransactionChangeEvent{
				EventType: models.BankTransactionUpdated, RequestID: transaction.OriginatingRequestID,
				MerchantID: "merchant", PaymentIntentID: transaction.PaymentIntentID, BankTransactionID: transaction.BankTransactionID,
				Changes: []models.BankTransactionFieldChange{{
					FieldName: "AuthorizationCode", OriginalValue: json.RawMessage(`null`), NewValue: json.RawMessage(`"private-bank-code"`),
				}},
			}))
			clock.Add(int64(time.Second))
			code := "private-bank-code"
			_, err = repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, &code)
			require.NoError(t, err)
			entries := make(logEntries, 16)
			service, err := NewAuditAndReconciliationService(capture, logging.NewConsole(entries))
			require.NoError(t, err)
			cancel, done := runWorker(t, service.Run)
			logs := readWorkerLogs(t, entries, 5)
			cancel()
			require.NoError(t, <-done)
			assert.Empty(t, entries)

			var info, warnings []map[string]interface{}
			for _, entry := range logs {
				if entry["level"] == "INFO" {
					info = append(info, entry)
				} else {
					warnings = append(warnings, entry)
				}
				assert.Equal(t, transaction.OriginatingRequestID, entry["request_id"])
				assert.Equal(t, "merchant", entry["merchant_id"])
				assert.Equal(t, transaction.BankTransactionID, entry["bank_transaction_id"])
				assert.Equal(t, transaction.PaymentIntentID, entry["payment_intent_id"])
				encoded, err := json.Marshal(entry)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), "private-bank-code")
				assert.NotContains(t, string(encoded), "2222405343241234")
				assert.NotContains(t, string(encoded), "cvv")
				assert.NotContains(t, entry["message"], "(null)")
			}
			require.Len(t, info, 4)
			require.Len(t, warnings, 1)
			events := make([]models.BankTransactionChangeEvent, len(info))
			for i, entry := range info {
				assert.EqualValues(t, i+1, entry["sequence"])
				require.NoError(t, json.Unmarshal([]byte(entry["message"].(string)), &events[i]))
				event := events[i]
				assert.EqualValues(t, i+1, event.Sequence)
				assert.Equal(t, entry["request_id"], event.RequestID)
				assert.Equal(t, entry["merchant_id"], event.MerchantID)
				assert.Equal(t, entry["payment_intent_id"], event.PaymentIntentID)
				assert.Equal(t, entry["bank_transaction_id"], event.BankTransactionID)
				assert.Equal(t, entry["event_type"], string(event.EventType))
				for _, field := range []string{"bank_transaction", "changes", "original_status", "new_status"} {
					assert.NotContains(t, entry, field, "audit details belong in the serialized event")
				}
				if i == 0 {
					assert.Equal(t, "banktransaction.created", entry["event_type"])
					require.NotNil(t, event.BankTransaction)
					assert.Equal(t, transaction.CardNumberLastFour, event.BankTransaction.CardNumberLastFour)
					assert.Equal(t, transaction.ExpiryMonth, event.BankTransaction.ExpiryMonth)
					assert.Equal(t, transaction.ExpiryYear, event.BankTransaction.ExpiryYear)
					assert.Equal(t, transaction.CreatedAt, event.BankTransaction.CreatedAt)
					assert.Equal(t, models.BankTransactionScheduled, event.BankTransaction.Status)
				} else {
					assert.Equal(t, "banktransaction.updated", entry["event_type"])
					if i == 2 {
						require.Len(t, event.Changes, 1)
						assert.Equal(t, "AuthorizationCode", event.Changes[0].FieldName)
						assert.JSONEq(t, `"[REDACTED]"`, string(event.Changes[0].NewValue))
					} else {
						require.GreaterOrEqual(t, len(event.Changes), 2)
						assert.Equal(t, "CompletedAt", event.Changes[1].FieldName)
						assert.NotEmpty(t, event.Changes[1].NewValue)
					}
				}
			}
			assert.JSONEq(t, `"Scheduled"`, string(events[1].Changes[0].OriginalValue))
			assert.JSONEq(t, `"`+string(failure)+`"`, string(events[1].Changes[0].NewValue))
			assert.JSONEq(t, `"`+string(failure)+`"`, string(events[3].Changes[0].OriginalValue))
			assert.JSONEq(t, `"Authorized"`, string(events[3].Changes[0].NewValue))
			assert.Equal(t, "WARN", warnings[0]["level"])
			assert.Equal(t, string(failure), warnings[0]["bank_transaction_status"])
			assert.EqualValues(t, 2, warnings[0]["sequence"])
			assert.Contains(t, warnings[0]["message"], "no verification API")
			assert.Contains(t, warnings[0]["message"], "no verification or payment retry was attempted")
			intent, err := repo.GetPaymentIntent("merchant", transaction.PaymentIntentID)
			require.NoError(t, err)
			assert.Equal(t, models.PaymentIntentAuthorized, intent.Status)
			assert.Equal(t, code, *intent.BankTransactions[0].AuthorizationCode)
		})
	}
}

type reconciliationLogExporter struct {
	records []sdklog.Record
}

func (e *reconciliationLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (*reconciliationLogExporter) Shutdown(context.Context) error   { return nil }
func (*reconciliationLogExporter) ForceFlush(context.Context) error { return nil }

func TestReconciliationExportsSerializedAuditWithoutMutatingEvent(t *testing.T) {
	exporter := &reconciliationLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	logger := slog.New(otelslog.NewHandler("test", otelslog.WithLoggerProvider(provider)))
	service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), logger)
	require.NoError(t, err)
	event := models.BankTransactionChangeEvent{
		EventType: models.BankTransactionUpdated, RequestID: "request", MerchantID: "merchant",
		PaymentIntentID: "intent", BankTransactionID: "transaction", Sequence: 2,
		Changes: []models.BankTransactionFieldChange{
			{FieldName: "Status", OriginalValue: json.RawMessage(`"Scheduled"`), NewValue: json.RawMessage(`"Authorized"`)},
			{FieldName: "CompletedAt", OriginalValue: json.RawMessage(`null`), NewValue: json.RawMessage(`"2030-04-01T12:00:00Z"`)},
			{FieldName: "AuthorizationCode", OriginalValue: json.RawMessage(`null`), NewValue: json.RawMessage(`"private-bank-code"`)},
		},
	}
	before, err := json.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, service.audit(context.Background(), event))
	require.Len(t, exporter.records, 1)
	record := exporter.records[0]
	var audited models.BankTransactionChangeEvent
	require.NoError(t, json.Unmarshal([]byte(record.Body().AsString()), &audited))
	expected := event
	expected.Changes = append([]models.BankTransactionFieldChange(nil), event.Changes...)
	expected.Changes[2].OriginalValue = json.RawMessage(`"[REDACTED]"`)
	expected.Changes[2].NewValue = json.RawMessage(`"[REDACTED]"`)
	assert.Equal(t, expected, audited)
	attrs := make(map[string]otellog.Value)
	record.WalkAttributes(func(attr otellog.KeyValue) bool {
		assert.NotContains(t, attrs, attr.Key)
		attrs[attr.Key] = attr.Value
		return true
	})
	assert.Equal(t, "request", attrs["request_id"].AsString())
	assert.Equal(t, "merchant", attrs["merchant_id"].AsString())
	assert.Equal(t, "intent", attrs["payment_intent_id"].AsString())
	assert.Equal(t, "transaction", attrs["bank_transaction_id"].AsString())
	assert.Equal(t, "banktransaction.updated", attrs["event_type"].AsString())
	assert.NotContains(t, attrs, "changes")
	assert.NotContains(t, record.Body().AsString(), "private-bank-code")
	after, err := json.Marshal(event)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestReconciliationOmitsUnavailableIDsAndRedactsUnknownChanges(t *testing.T) {
	entries := make(logEntries, 1)
	service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), logging.NewConsole(entries))
	require.NoError(t, err)
	require.NoError(t, service.audit(context.Background(), models.BankTransactionChangeEvent{
		EventType: models.BankTransactionUpdated, MerchantID: "merchant", Sequence: 1,
		Changes: []models.BankTransactionFieldChange{{
			FieldName: "OtherField", OriginalValue: json.RawMessage(`"private-before"`), NewValue: json.RawMessage(`"private-after"`),
		}},
	}))
	entry := readWorkerLogs(t, entries, 1)[0]
	var event models.BankTransactionChangeEvent
	require.NoError(t, json.Unmarshal([]byte(entry["message"].(string)), &event))
	assert.Equal(t, "merchant", entry["merchant_id"])
	for _, field := range []string{"request_id", "payment_intent_id", "bank_transaction_id", "original_status", "new_status"} {
		assert.NotContains(t, entry, field)
	}
	require.Len(t, event.Changes, 1)
	assert.Equal(t, "OtherField", event.Changes[0].FieldName)
	assert.JSONEq(t, `"[REDACTED]"`, string(event.Changes[0].OriginalValue))
	assert.JSONEq(t, `"[REDACTED]"`, string(event.Changes[0].NewValue))
}

func TestReconciliationRejectsMalformedCompletionDeltaWithoutLogging(t *testing.T) {
	entries := make(logEntries, 1)
	service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), logging.NewConsole(entries))
	require.NoError(t, err)
	err = service.audit(context.Background(), models.BankTransactionChangeEvent{
		EventType: models.BankTransactionUpdated,
		Changes: []models.BankTransactionFieldChange{{
			FieldName: "CompletedAt", OriginalValue: json.RawMessage(`null`), NewValue: json.RawMessage(`{`),
		}},
	})
	require.ErrorContains(t, err, "serialize bank transaction audit event")
	assert.Empty(t, entries)
}

func TestReconciliationAuditRedactsSnapshotWithoutMutation(t *testing.T) {
	entries := make(logEntries, 1)
	service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), logging.NewConsole(entries))
	require.NoError(t, err)
	code := "private-bank-code"
	event := models.BankTransactionChangeEvent{
		EventType: models.BankTransactionCreated, RequestID: "request", MerchantID: "merchant",
		PaymentIntentID: "intent", BankTransactionID: "transaction",
		BankTransaction: &models.BankTransaction{
			BankTransactionID: "transaction", PaymentIntentID: "intent", MerchantID: "merchant",
			Status: models.BankTransactionScheduled, CardNumberLastFour: "1234",
			AuthorizationCode: &code, IdempotencyKey: "private-key", RequestFingerprint: "private-fingerprint",
		},
	}
	require.NoError(t, service.audit(context.Background(), event))
	entry := readWorkerLogs(t, entries, 1)[0]
	message := entry["message"].(string)
	var audited models.BankTransactionChangeEvent
	require.NoError(t, json.Unmarshal([]byte(message), &audited))
	require.NotNil(t, audited.BankTransaction)
	assert.Nil(t, audited.BankTransaction.AuthorizationCode)
	assert.Equal(t, "1234", audited.BankTransaction.CardNumberLastFour)
	assert.Equal(t, &code, event.BankTransaction.AuthorizationCode)
	assert.Equal(t, "private-key", event.BankTransaction.IdempotencyKey)
	for _, secret := range []string{code, "private-key", "private-fingerprint"} {
		assert.NotContains(t, message, secret)
	}
}

func TestReconcileChecksUpdatedStatusBeforeWarning(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventType models.BankTransactionEventType
		field     string
		value     json.RawMessage
		warn      bool
		invalid   bool
	}{
		{"created", models.BankTransactionCreated, "Status", json.RawMessage(`"RequestTimeout"`), false, false},
		{"non-status update", models.BankTransactionUpdated, "CompletedAt", json.RawMessage(`null`), false, false},
		{"authorized", models.BankTransactionUpdated, "Status", json.RawMessage(`"Authorized"`), false, false},
		{"declined", models.BankTransactionUpdated, "Status", json.RawMessage(`"Declined"`), false, false},
		{"requires verification", models.BankTransactionUpdated, "Status", json.RawMessage(`"RequestTimeout"`), true, false},
		{"invalid status", models.BankTransactionUpdated, "Status", json.RawMessage(`123`), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := make(logEntries, 1)
			service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), logging.NewConsole(entries))
			require.NoError(t, err)
			err = service.reconcile(context.Background(), models.BankTransactionChangeEvent{
				EventType: test.eventType, MerchantID: "merchant", PaymentIntentID: "intent", BankTransactionID: "transaction",
				Changes: []models.BankTransactionFieldChange{{FieldName: test.field, NewValue: test.value}},
			})
			if test.invalid {
				require.ErrorContains(t, err, "decode new bank transaction status")
			} else {
				require.NoError(t, err)
			}
			if test.warn {
				entry := readWorkerLogs(t, entries, 1)[0]
				assert.Equal(t, "WARN", entry["level"])
				assert.Equal(t, "RequestTimeout", entry["bank_transaction_status"])
				assert.Equal(t, "transaction", entry["bank_transaction_id"])
			}
			assert.Empty(t, entries)
		})
	}
}

func TestBankAuditFailureStopsBeforeReconciliation(t *testing.T) {
	capture := changedatacapture.NewInMemory[models.BankTransactionChangeEvent]()
	require.NoError(t, capture.Produce(models.BankTransactionChangeEvent{
		EventType: models.BankTransactionUpdated,
		Changes: []models.BankTransactionFieldChange{
			{FieldName: "Status", OriginalValue: json.RawMessage(`"Scheduled"`), NewValue: json.RawMessage(`"RequestTimeout"`)},
			{FieldName: "CompletedAt", OriginalValue: json.RawMessage(`null`), NewValue: json.RawMessage(`{`)},
		},
	}))
	entries := make(logEntries, 2)
	service, err := NewAuditAndReconciliationService(capture, logging.NewConsole(entries))
	require.NoError(t, err)
	require.ErrorContains(t, service.Run(context.Background()), "serialize bank transaction audit event")
	assert.Empty(t, entries)
}

func TestReconciliationLogsHistoricalEventsWithoutChangingCurrentState(t *testing.T) {
	repo, capture, _ := workerRepository(t)
	transaction := workerTransaction(t, repo)
	_, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionRequestTimeout, nil)
	require.NoError(t, err)
	_, err = repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, nil)
	require.NoError(t, err)
	historical := models.BankTransactionChangeEvent{
		EventType: models.BankTransactionUpdated, RequestID: transaction.OriginatingRequestID,
		MerchantID: "merchant", PaymentIntentID: transaction.PaymentIntentID, BankTransactionID: transaction.BankTransactionID,
		Changes: []models.BankTransactionFieldChange{{
			FieldName: "Status", OriginalValue: json.RawMessage(`"Scheduled"`), NewValue: json.RawMessage(`"RequestTimeout"`),
		}},
	}
	require.NoError(t, capture.Produce(historical))
	require.NoError(t, capture.Produce(historical))
	entries := make(logEntries, 16)
	service, err := NewAuditAndReconciliationService(capture, logging.NewConsole(entries))
	require.NoError(t, err)
	cancel, done := runWorker(t, service.Run)
	logs := readWorkerLogs(t, entries, 8)
	cancel()
	require.NoError(t, <-done)
	warnings := 0
	for _, entry := range logs {
		if entry["level"] == "WARN" {
			warnings++
		}
	}
	assert.Equal(t, 3, warnings)
	intent, err := repo.GetPaymentIntent("merchant", transaction.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentAuthorized, intent.Status)
}

type failedConsumer struct{ err error }

func (f failedConsumer) Produce(models.BankTransactionChangeEvent) error {
	return errors.New("unexpected publication")
}
func (f failedConsumer) Consume(context.Context) (models.BankTransactionChangeEvent, error) {
	return models.BankTransactionChangeEvent{}, f.err
}

func TestReconciliationPropagatesFailedOrUnexpectedlyStoppedConsumers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, failure := range []error{errors.New("consumer failed"), io.EOF, context.Canceled} {
		service, err := NewAuditAndReconciliationService(failedConsumer{failure}, logger)
		require.NoError(t, err)
		assert.ErrorIs(t, service.Run(context.Background()), failure)
	}
}

type consumptionContext struct {
	context.Context
	waiting chan struct{}
}

func (c consumptionContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestReconciliationStopsCleanlyWhileWaitingForEvents(t *testing.T) {
	service, err := NewAuditAndReconciliationService(changedatacapture.NewInMemory[models.BankTransactionChangeEvent](), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	waiting := make(chan struct{}, 1)
	cancel, done := runWorker(t, func(ctx context.Context) error {
		return service.Run(consumptionContext{Context: ctx, waiting: waiting})
	})
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never waited for an event")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker failed to stop after cancellation")
	}
}
