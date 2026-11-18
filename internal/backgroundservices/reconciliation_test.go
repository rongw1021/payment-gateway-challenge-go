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
			service, err := NewReconciliationService(capture, logging.NewConsole(entries))
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
			for i, entry := range info {
				assert.EqualValues(t, i+1, entry["sequence"])
				message, ok := entry["message"].(string)
				require.True(t, ok)
				var event models.BankTransactionChangeEvent
				require.NoError(t, json.Unmarshal([]byte(message), &event))
				assert.EqualValues(t, i+1, event.Sequence)
				assert.Equal(t, transaction.BankTransactionID, event.BankTransactionID)
				assert.Equal(t, transaction.PaymentIntentID, event.PaymentIntentID)
				assert.Equal(t, transaction.OriginatingRequestID, event.RequestID)
				if i == 0 {
					require.NotNil(t, event.BankTransaction)
					assert.Equal(t, models.BankTransactionScheduled, event.BankTransaction.Status)
				}
				if i == 2 {
					require.Len(t, event.Changes, 1)
					assert.Equal(t, "AuthorizationCode", event.Changes[0].FieldName)
					assert.JSONEq(t, `"[REDACTED]"`, string(event.Changes[0].NewValue))
				}
			}
			assert.Equal(t, "Scheduled", info[0]["new_status"])
			assert.NotContains(t, info[0], "original_status")
			assert.Equal(t, "Scheduled", info[1]["original_status"])
			assert.Equal(t, string(failure), info[1]["new_status"])
			assert.NotContains(t, info[2], "original_status")
			assert.NotContains(t, info[2], "new_status")
			assert.Equal(t, string(failure), info[3]["original_status"])
			assert.Equal(t, "Authorized", info[3]["new_status"])
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
	service, err := NewReconciliationService(capture, logging.NewConsole(entries))
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
		service, err := NewReconciliationService(failedConsumer{failure}, logger)
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
	service, err := NewReconciliationService(changedatacapture.NewInMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
