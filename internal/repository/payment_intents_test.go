package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	intentCardToken      = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	otherIntentCardToken = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

type intentChanges struct {
	mu      sync.Mutex
	events  []models.BankTransactionChangeEvent
	observe func(models.BankTransactionChangeEvent)
	err     error
}

type paymentIntentChanges struct {
	events []models.PaymentIntentChangeEvent
	err    error
}

func (c *paymentIntentChanges) Produce(event models.PaymentIntentChangeEvent) error {
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, event.WithSequence(int64(len(c.events)+1)))
	return nil
}

func (c *intentChanges) Produce(event models.BankTransactionChangeEvent) error {
	if c.observe != nil {
		c.observe(event)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	event.Sequence = int64(len(c.events) + 1)
	c.events = append(c.events, event)
	return nil
}

func intentRepository(t *testing.T) (*PaymentIntentsRepository, *intentChanges, *atomic.Int64) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	changes := &intentChanges{}
	repo, err := NewPaymentIntentsRepository(changes, changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](), func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	require.NoError(t, err)
	return repo, changes, clock
}

func createIntent(t *testing.T, repo *PaymentIntentsRepository, merchant, key string) models.PaymentIntent {
	t.Helper()
	amount := int32(100)
	intent, hasConflict, err := repo.CreatePaymentIntent(merchant, key, "creation-request", models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	require.False(t, hasConflict)
	return intent
}

func consumeIntentCard() (*models.CreateCardTokenRequest, error) {
	return &models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 2035, Cvv: "012"}, nil
}

func createIntentTransaction(t *testing.T, repo *PaymentIntentsRepository, intent models.PaymentIntent, key string) models.BankTransaction {
	t.Helper()
	current, transaction, card, err := repo.CreateBankTransaction(intent.MerchantID, intent.PaymentIntentID, key, "original-request",
		models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}, consumeIntentCard)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.NotNil(t, transaction)
	require.NotNil(t, card)
	return *transaction
}

func TestPaymentIntentCreationIsMerchantScopedAndConcurrentIdempotent(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intentEvents := &paymentIntentChanges{}
	repo.paymentIntentChange = intentEvents
	amount := int32(100)
	request := models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount}
	results := make(chan models.PaymentIntent, 16)
	var wg sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent, hasConflict, err := repo.CreatePaymentIntent("merchant", "private-key", "creation-request", request)
			assert.NoError(t, err)
			assert.False(t, hasConflict)
			results <- intent
		}()
	}
	wg.Wait()
	close(results)
	var id string
	for intent := range results {
		if id == "" {
			id = intent.PaymentIntentID
		}
		assert.Equal(t, id, intent.PaymentIntentID)
		assert.Empty(t, intent.BankTransactions)
		assert.NotNil(t, intent.BankTransactions)
	}
	assert.Len(t, repo.paymentIntents, 1)
	require.Len(t, intentEvents.events, 1, "concurrent matching creations emit one event")
	assert.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", id)
	for _, changed := range []models.CreatePaymentIntentRequest{
		{Currency: "USD", Amount: &amount},
		{Currency: "GBP", Amount: new(int32)},
	} {
		intent, hasConflict, err := repo.CreatePaymentIntent("merchant", "private-key", "creation-request", changed)
		require.NoError(t, err)
		assert.True(t, hasConflict)
		assert.Equal(t, int32(100), intent.Amount)
		assert.Equal(t, "GBP", intent.Currency)
	}
	other := createIntent(t, repo, "other", "private-key")
	assert.NotEqual(t, id, other.PaymentIntentID)
	missing, err := repo.GetPaymentIntent("other", id)
	require.NoError(t, err)
	assert.Nil(t, missing)
	assert.Empty(t, changes.events)
	require.Len(t, intentEvents.events, 2, "only the two merchants' creations emit intent events")
}

func TestPaymentIntentEventsTrackActualStateChangesAndTimestamps(t *testing.T) {
	repo, bankEvents, clock := intentRepository(t)
	intentEvents := &paymentIntentChanges{}
	repo.paymentIntentChange = intentEvents
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	intent := createIntent(t, repo, "merchant", "private-create-key")
	assert.Equal(t, now(), intent.CreatedAt)
	assert.Equal(t, intent.CreatedAt, intent.UpdatedAt)
	require.Len(t, intentEvents.events, 1)
	createdEvent := intentEvents.events[0]
	assert.Equal(t, models.PaymentIntentCreatedEvent, createdEvent.EventType)
	assert.Equal(t, "creation-request", createdEvent.RequestID)
	require.NotNil(t, createdEvent.PaymentIntent)
	assert.Equal(t, intent.CreatedAt, createdEvent.PaymentIntent.CreatedAt)
	assert.Equal(t, int32(100), createdEvent.PaymentIntent.Amount)
	assert.Equal(t, "GBP", createdEvent.PaymentIntent.Currency)
	originalEventJSON, err := json.Marshal(createdEvent)
	require.NoError(t, err)

	clock.Add(int64(time.Second))
	first := createIntentTransaction(t, repo, intent, "first")
	assert.Equal(t, now(), first.CreatedAt)
	assert.Equal(t, first.CreatedAt, first.UpdatedAt)
	clock.Add(int64(time.Second))
	_, err = repo.CompleteBankTransaction("merchant", first.BankTransactionID, models.BankTransactionDeclined, nil)
	require.NoError(t, err)
	clock.Add(int64(time.Second))
	second := createIntentTransaction(t, repo, intent, "second")
	clock.Add(int64(30 * time.Second))
	marked, err := repo.TryMarkTimedOutBankTransaction("merchant", second.BankTransactionID, now())
	require.NoError(t, err)
	assert.True(t, marked)
	pending, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, now(), pending.UpdatedAt)
	assert.Equal(t, now(), pending.BankTransactions[1].UpdatedAt)
	require.Len(t, intentEvents.events, 5)

	clock.Add(int64(time.Second))
	_, err = repo.CompleteBankTransaction("merchant", second.BankTransactionID, models.BankTransactionNetworkError, nil)
	require.NoError(t, err)
	stillPending, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, pending.UpdatedAt, stillPending.UpdatedAt)
	assert.Equal(t, now(), stillPending.BankTransactions[1].UpdatedAt)
	assert.Equal(t, second.CreatedAt, stillPending.BankTransactions[1].CreatedAt)
	assert.Len(t, intentEvents.events, 5, "a changed bank uncertainty reason is not a changed intent state")
	assert.Len(t, bankEvents.events, 5, "the independent bank event is still published")

	clock.Add(int64(time.Second))
	_, err = repo.CompleteBankTransaction("merchant", second.BankTransactionID, models.BankTransactionAuthorized, nil)
	require.NoError(t, err)
	current, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, intent.CreatedAt, current.CreatedAt)
	assert.Equal(t, now(), current.UpdatedAt)
	assert.Equal(t, now(), current.BankTransactions[1].UpdatedAt)
	require.Len(t, intentEvents.events, 6)
	statuses := []models.PaymentIntentStatus{
		models.PaymentIntentCreated, models.PaymentIntentProcessing, models.PaymentIntentDeclined,
		models.PaymentIntentProcessing, models.PaymentIntentPendingVerification, models.PaymentIntentAuthorized,
	}
	for i, event := range intentEvents.events {
		assert.EqualValues(t, i+1, event.Sequence)
		assert.Equal(t, "merchant", event.MerchantID)
		assert.Equal(t, intent.PaymentIntentID, event.PaymentIntentID)
		if i == 0 {
			continue
		}
		assert.Equal(t, models.PaymentIntentUpdatedEvent, event.EventType)
		assert.Equal(t, "original-request", event.RequestID)
		assert.Nil(t, event.PaymentIntent)
		require.Len(t, event.Changes, 2)
		assert.Equal(t, "Status", event.Changes[0].FieldName)
		assert.JSONEq(t, fmt.Sprintf("%q", statuses[i-1]), string(event.Changes[0].OriginalValue))
		assert.JSONEq(t, fmt.Sprintf("%q", statuses[i]), string(event.Changes[0].NewValue))
		assert.Equal(t, "UpdatedAt", event.Changes[1].FieldName)
	}

	clock.Add(int64(time.Minute))
	replay := createIntent(t, repo, "merchant", "private-create-key")
	assert.Equal(t, current.UpdatedAt, replay.UpdatedAt)
	assert.Equal(t, current.BankTransactions, replay.BankTransactions)
	_, _, _, err = repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "first", "replay-request",
		models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}, func() (*models.CreateCardTokenRequest, error) {
			t.Fatal("historical replay must not consume a token")
			return nil, nil
		})
	require.NoError(t, err)
	_, _, _, err = repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "new", "request",
		models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}, consumeIntentCard)
	assert.ErrorIs(t, err, models.ErrIntentNotPayable)
	_, err = repo.CompleteBankTransaction("merchant", second.BankTransactionID, models.BankTransactionAuthorized, nil)
	require.NoError(t, err)
	assert.Len(t, intentEvents.events, 6)
	unchanged, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, current.UpdatedAt, unchanged.UpdatedAt)
	assert.Equal(t, current.BankTransactions, unchanged.BankTransactions)
	historicalJSON, err := json.Marshal(createdEvent)
	require.NoError(t, err)
	assert.Equal(t, originalEventJSON, historicalJSON)
	allEvents, err := json.Marshal(intentEvents.events)
	require.NoError(t, err)
	for _, private := range []string{"private-create-key", intentCardToken, "bank_transactions", "idempotency_key", "fingerprint", "cvv", "0222405343240007"} {
		assert.NotContains(t, string(allEvents), private)
	}
}

func TestPaymentIntentPublicationFailuresRetainStateAndAttemptBothStreams(t *testing.T) {
	for _, stage := range []string{"create", "reserve", "complete"} {
		t.Run(stage, func(t *testing.T) {
			repo, bankEvents, clock := intentRepository(t)
			intentEvents := &paymentIntentChanges{}
			repo.paymentIntentChange = intentEvents
			intentFailure := errors.New("intent event publication failed")
			if stage == "create" {
				intentEvents.err = intentFailure
				amount := int32(100)
				_, _, err := repo.CreatePaymentIntent("merchant", "key", "request", models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
				require.ErrorIs(t, err, intentFailure)
				require.Len(t, repo.paymentIntents, 1)
				stored := repo.paymentIntents[0]
				clock.Add(int64(time.Minute))
				replay := createIntent(t, repo, "merchant", "key")
				assert.Equal(t, stored.CreatedAt, replay.CreatedAt)
				assert.Equal(t, stored.UpdatedAt, replay.UpdatedAt)
				assert.Empty(t, intentEvents.events)
				return
			}
			intent := createIntent(t, repo, "merchant", "key")
			var transaction models.BankTransaction
			if stage == "complete" {
				transaction = createIntentTransaction(t, repo, intent, "confirm")
			}
			intentEvents.err = intentFailure
			if stage == "reserve" {
				_, _, _, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "confirm", "request",
					models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}, consumeIntentCard)
				require.ErrorIs(t, err, intentFailure)
				assert.Len(t, bankEvents.events, 1)
				assert.Equal(t, models.PaymentIntentProcessing, repo.paymentIntents[0].Status)
			} else {
				bankFailure := errors.New("bank event publication failed")
				bankEvents.err = bankFailure
				_, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, nil)
				require.ErrorIs(t, err, bankFailure)
				require.ErrorIs(t, err, intentFailure, "both publications must be attempted")
				assert.Equal(t, models.PaymentIntentAuthorized, repo.paymentIntents[0].Status)
			}
		})
	}
}

func TestPaymentIntentReservationChecksPrecedeCardConsumption(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "create")
	request := models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}
	calls := 0
	consume := func() (*models.CreateCardTokenRequest, error) {
		calls++
		return consumeIntentCard()
	}
	for _, attempt := range []struct{ merchant, id string }{{"other", intent.PaymentIntentID}, {"merchant", "missing"}} {
		current, transaction, card, err := repo.CreateBankTransaction(attempt.merchant, attempt.id, "pay", "request", request, consume)
		assert.ErrorIs(t, err, models.ErrPaymentIntentNotFound)
		assert.Nil(t, current)
		assert.Nil(t, transaction)
		assert.Nil(t, card)
	}
	assert.Zero(t, calls)
	_, first, _, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "pay", "original", request, consume)
	require.NoError(t, err)
	_, replay, card, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "pay", "retry", request, consume)
	require.NoError(t, err)
	assert.Nil(t, card)
	assert.Equal(t, first, replay)
	assert.Equal(t, "original", replay.OriginatingRequestID)
	current, transaction, card, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "pay", "request",
		models.ConfirmPaymentIntentRequest{CardToken: otherIntentCardToken}, consume)
	assert.ErrorIs(t, err, models.ErrIdempotencyConflict)
	require.NotNil(t, current)
	assert.Equal(t, models.PaymentIntentProcessing, current.Status)
	assert.Nil(t, transaction)
	assert.Nil(t, card)
	current, transaction, card, err = repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "different-key", "request", request, consume)
	assert.ErrorIs(t, err, models.ErrIntentNotPayable)
	require.NotNil(t, current)
	assert.Equal(t, models.PaymentIntentProcessing, current.Status)
	assert.Nil(t, transaction)
	assert.Nil(t, card)
	another := createIntent(t, repo, "merchant", "another")
	current, _, _, err = repo.CreateBankTransaction("merchant", another.PaymentIntentID, "pay", "request", request, consume)
	assert.ErrorIs(t, err, models.ErrIdempotencyConflict)
	require.NotNil(t, current)
	assert.Equal(t, another.PaymentIntentID, current.PaymentIntentID)
	assert.Equal(t, models.PaymentIntentCreated, current.Status)
	assert.Equal(t, 1, calls)
	assert.Len(t, changes.events, 1)
	foreign, err := repo.GetBankTransaction("other", first.BankTransactionID)
	require.NoError(t, err)
	assert.Nil(t, foreign)
	_, err = repo.CompleteBankTransaction("other", first.BankTransactionID, models.BankTransactionAuthorized, nil)
	assert.ErrorIs(t, err, ErrBankTransactionNotFound)

	failure := errors.New("card store unavailable")
	for _, test := range []struct {
		consumer     func() (*models.CreateCardTokenRequest, error)
		wantErr      error
		wantSnapshot bool
	}{
		{func() (*models.CreateCardTokenRequest, error) { return nil, nil }, models.ErrInvalidCardToken, true},
		{func() (*models.CreateCardTokenRequest, error) { return nil, failure }, failure, false},
	} {
		current, transaction, card, err := repo.CreateBankTransaction("merchant", another.PaymentIntentID, "retry", "request", request, test.consumer)
		require.ErrorIs(t, err, test.wantErr)
		assert.Nil(t, transaction)
		assert.Nil(t, card)
		if test.wantSnapshot {
			require.NotNil(t, current)
			assert.Equal(t, models.PaymentIntentCreated, current.Status)
		} else {
			assert.Nil(t, current)
		}
		current, err = repo.GetPaymentIntent("merchant", another.PaymentIntentID)
		require.NoError(t, err)
		assert.Equal(t, models.PaymentIntentCreated, current.Status)
		assert.Empty(t, current.BankTransactions)
	}
	createIntentTransaction(t, repo, another, "retry")
}

func TestPaymentIntentInvalidTokenReturnsDeclinedSnapshot(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "create")
	first := createIntentTransaction(t, repo, intent, "first")
	_, err := repo.CompleteBankTransaction("merchant", first.BankTransactionID, models.BankTransactionDeclined, nil)
	require.NoError(t, err)

	current, transaction, card, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "retry", "request",
		models.ConfirmPaymentIntentRequest{CardToken: otherIntentCardToken},
		func() (*models.CreateCardTokenRequest, error) { return nil, nil })

	assert.ErrorIs(t, err, models.ErrInvalidCardToken)
	require.NotNil(t, current)
	assert.Equal(t, models.PaymentIntentDeclined, current.Status)
	require.Len(t, current.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionDeclined, current.BankTransactions[0].Status)
	assert.Nil(t, transaction)
	assert.Nil(t, card)
	assert.Len(t, changes.events, 2)
	current.BankTransactions[0].Status = models.BankTransactionAuthorized
	retry := createIntentTransaction(t, repo, intent, "retry")
	current, err = repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.BankTransactionDeclined, current.BankTransactions[0].Status)
	assert.Equal(t, models.BankTransactionScheduled, retry.Status)
}

func TestBankTransactionCompletionPublishesAtomicSnapshots(t *testing.T) {
	for _, status := range []models.BankTransactionStatus{
		models.BankTransactionAuthorized, models.BankTransactionDeclined,
		models.BankTransactionRequestTimeout, models.BankTransactionStatusUpdateTimeout,
		models.BankTransactionServiceUnavailable, models.BankTransactionNetworkError,
		models.BankTransactionInvalidResponse, models.BankTransactionRequestCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			repo, changes, clock := intentRepository(t)
			observed := 0
			changes.observe = func(event models.BankTransactionChangeEvent) {
				// Publication holds the mutex; inspect committed state without re-entering it.
				intentIndex := repo.findPaymentIntent(event.MerchantID, event.PaymentIntentID)
				transactionIndex := repo.findBankTransaction(event.MerchantID, event.BankTransactionID)
				require.NotEqual(t, -1, intentIndex)
				require.NotEqual(t, -1, transactionIndex)
				intent := repo.withBankTransactions(repo.paymentIntents[intentIndex])
				transaction := repo.bankTransactions[transactionIndex]
				require.Len(t, intent.BankTransactions, 1)
				assert.Equal(t, transaction, intent.BankTransactions[0])
				expected, err := transaction.Status.ToPaymentIntentStatus()
				require.NoError(t, err)
				assert.Equal(t, expected, intent.Status)
				observed++
			}
			intent := createIntent(t, repo, "merchant", "create")
			scheduled := createIntentTransaction(t, repo, intent, "pay")
			clock.Add(int64(time.Second))
			code := "bank-code"
			completed, err := repo.CompleteBankTransaction("merchant", scheduled.BankTransactionID, status, &code)
			require.NoError(t, err)
			assert.Equal(t, status, completed.Status)
			assert.Equal(t, time.Unix(0, clock.Load()).UTC(), completed.UpdatedAt)
			assert.Equal(t, scheduled.CreatedAt, completed.CreatedAt)
			assert.Equal(t, scheduled.CreatedAt, scheduled.UpdatedAt)
			assert.Equal(t, models.PaymentIntentCreated, intent.Status)
			assert.Equal(t, 2, observed)
			require.Len(t, changes.events, 2)
			assert.Equal(t, models.BankTransactionCreated, changes.events[0].EventType)
			assert.Empty(t, changes.events[0].Changes)
			require.NotNil(t, changes.events[0].BankTransaction)
			assert.Equal(t, scheduled.UpdatedAt, changes.events[0].BankTransaction.UpdatedAt)
			update := changes.events[1]
			assert.Nil(t, update.BankTransaction)
			require.Len(t, update.Changes, 3)
			assert.Equal(t, "Status", update.Changes[0].FieldName)
			assert.JSONEq(t, `"Scheduled"`, string(update.Changes[0].OriginalValue))
			assert.JSONEq(t, fmt.Sprintf("%q", status), string(update.Changes[0].NewValue))
			assert.Equal(t, "UpdatedAt", update.Changes[1].FieldName)
			before, err := json.Marshal(scheduled.UpdatedAt)
			require.NoError(t, err)
			after, err := json.Marshal(completed.UpdatedAt)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(update.Changes[1].OriginalValue))
			assert.JSONEq(t, string(after), string(update.Changes[1].NewValue))
			assert.Equal(t, "original-request", update.RequestID)
			clock.Add(int64(time.Second))
			replay, err := repo.CompleteBankTransaction("merchant", completed.BankTransactionID, status, &code)
			require.NoError(t, err)
			assert.Equal(t, completed, replay)
			assert.Len(t, changes.events, 2)
			_, err = repo.CompleteBankTransaction("merchant", completed.BankTransactionID, status, nil)
			require.Error(t, err)
		})
	}
}

func TestBankTransactionSameClockStatusChangeOmitsUnchangedTimestampDelta(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "create")
	scheduled := createIntentTransaction(t, repo, intent, "pay")
	declined, err := repo.CompleteBankTransaction("merchant", scheduled.BankTransactionID, models.BankTransactionDeclined, nil)
	require.NoError(t, err)
	assert.Equal(t, scheduled.UpdatedAt, declined.UpdatedAt)
	require.Len(t, changes.events, 2)
	require.Len(t, changes.events[1].Changes, 1)
	assert.Equal(t, "Status", changes.events[1].Changes[0].FieldName)
}

func TestPaymentIntentHistoryCopiesAndOldOutcomesCannotRegressRetries(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, 0, time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			repo, changes, clock := intentRepository(t)
			intent := createIntent(t, repo, "merchant", "create")
			first := createIntentTransaction(t, repo, intent, "first")
			scheduled, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			_, err = repo.CompleteBankTransaction("merchant", first.BankTransactionID, models.BankTransactionScheduled, nil)
			require.Error(t, err)
			declined, err := repo.CompleteBankTransaction("merchant", first.BankTransactionID, models.BankTransactionDeclined, nil)
			require.NoError(t, err)
			clock.Add(int64(offset))
			second := createIntentTransaction(t, repo, intent, "second")
			snapshot, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			require.Len(t, snapshot.BankTransactions, 2)
			expected := []string{first.BankTransactionID, second.BankTransactionID}
			if offset < 0 {
				expected[0], expected[1] = expected[1], expected[0]
			}
			assert.Equal(t, expected, []string{snapshot.BankTransactions[0].BankTransactionID, snapshot.BankTransactions[1].BankTransactionID})
			replay, err := repo.CompleteBankTransaction("merchant", declined.BankTransactionID, models.BankTransactionDeclined, nil)
			require.NoError(t, err)
			assert.Equal(t, declined, replay)
			_, err = repo.CompleteBankTransaction("merchant", declined.BankTransactionID, models.BankTransactionAuthorized, nil)
			require.Error(t, err)
			changed, err := repo.TryMarkTimedOutBankTransaction("merchant", declined.BankTransactionID, time.Unix(0, clock.Load()).Add(time.Hour))
			require.NoError(t, err)
			assert.False(t, changed)
			code := "bank-code"
			completed, err := repo.CompleteBankTransaction("merchant", second.BankTransactionID, models.BankTransactionAuthorized, &code)
			require.NoError(t, err)
			code = "mutated-input"
			*completed.AuthorizationCode = "mutated-output"
			completed.UpdatedAt = time.Time{}
			snapshot.BankTransactions[0].CardNumberLastFour = "9999"
			declined.UpdatedAt = time.Time{}
			changes.events[0].BankTransaction.CardNumberLastFour = "8888"
			current, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			assert.Equal(t, models.PaymentIntentAuthorized, current.Status)
			assert.Equal(t, models.BankTransactionScheduled, scheduled.BankTransactions[0].Status)
			assert.Equal(t, first.CreatedAt, scheduled.BankTransactions[0].UpdatedAt)
			for _, transaction := range current.BankTransactions {
				assert.Equal(t, "0007", transaction.CardNumberLastFour)
				assert.False(t, transaction.UpdatedAt.IsZero())
				if transaction.Status == models.BankTransactionAuthorized {
					assert.Equal(t, "bank-code", *transaction.AuthorizationCode)
				}
			}
		})
	}
}

func TestBankTransactionTimeoutBoundaryAndLateDecisions(t *testing.T) {
	for _, late := range []models.BankTransactionStatus{
		models.BankTransactionRequestTimeout, models.BankTransactionServiceUnavailable,
		models.BankTransactionNetworkError, models.BankTransactionInvalidResponse, models.BankTransactionRequestCanceled,
	} {
		t.Run(string(late), func(t *testing.T) {
			repo, changes, clock := intentRepository(t)
			intent := createIntent(t, repo, "merchant", "create")
			transaction := createIntentTransaction(t, repo, intent, "pay")
			marked, err := repo.TryMarkTimedOutBankTransaction("merchant", transaction.BankTransactionID, transaction.CreatedAt.Add(-time.Nanosecond))
			require.NoError(t, err)
			assert.False(t, marked)
			clock.Add(int64(30 * time.Second))
			marked, err = repo.TryMarkTimedOutBankTransaction("merchant", transaction.BankTransactionID, transaction.CreatedAt)
			require.NoError(t, err)
			assert.True(t, marked)
			require.Len(t, changes.events[1].Changes, 2)
			assert.Equal(t, "UpdatedAt", changes.events[1].Changes[1].FieldName)
			clock.Add(int64(time.Second))
			refined, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, late, nil)
			require.NoError(t, err)
			assert.Equal(t, late, refined.Status)
			assert.Equal(t, time.Unix(0, clock.Load()).UTC(), refined.UpdatedAt)
			_, _, _, err = repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "new", "request",
				models.ConfirmPaymentIntentRequest{CardToken: otherIntentCardToken}, consumeIntentCard)
			assert.ErrorIs(t, err, models.ErrIntentNotPayable)
			_, err = repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, nil)
			require.NoError(t, err)
			_, err = repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, late, nil)
			require.Error(t, err)
			marked, err = repo.TryMarkTimedOutBankTransaction("merchant", transaction.BankTransactionID, transaction.CreatedAt)
			require.NoError(t, err)
			assert.False(t, marked)
		})
	}
}

func TestBankTransactionTimeoutQueryIsBoundedAndDeterministic(t *testing.T) {
	repo, _, clock := intentRepository(t)
	var eligible []models.BankTransaction
	for _, key := range []string{"one", "two", "declined"} {
		intent := createIntent(t, repo, "merchant", key)
		transaction := createIntentTransaction(t, repo, intent, key)
		if key == "declined" {
			_, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionDeclined, nil)
			require.NoError(t, err)
		} else {
			eligible = append(eligible, transaction)
		}
	}
	cutoff := time.Unix(0, clock.Load()).UTC()
	clock.Add(1)
	fresh := createIntent(t, repo, "merchant", "fresh")
	createIntentTransaction(t, repo, fresh, "fresh")
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].BankTransactionID < eligible[j].BankTransactionID })
	found, err := repo.GetTimedOutBankTransactions(cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, eligible, found)
	found, err = repo.GetTimedOutBankTransactions(cutoff.Add(-time.Nanosecond), 100)
	require.NoError(t, err)
	assert.Empty(t, found)
	found, err = repo.GetTimedOutBankTransactions(cutoff, 1)
	require.NoError(t, err)
	assert.Equal(t, eligible[:1], found)
	_, err = repo.GetTimedOutBankTransactions(cutoff, 0)
	require.Error(t, err)
}

func TestBankTransactionAuthoritativeCompletionWinsConcurrentTimeout(t *testing.T) {
	repo, _, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "create")
	transaction := createIntentTransaction(t, repo, intent, "pay")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.TryMarkTimedOutBankTransaction("merchant", transaction.BankTransactionID, transaction.CreatedAt)
		assert.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		_, err := repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, nil)
		assert.NoError(t, err)
	}()
	wg.Wait()
	current, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentAuthorized, current.Status)
	assert.Equal(t, models.BankTransactionAuthorized, current.BankTransactions[0].Status)
}

func TestPaymentIntentReservationPublicationFailureRetainsCommittedAttempt(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "create")
	failure := errors.New("event transport unavailable")
	changes.err = failure
	consumed := 0
	consume := func() (*models.CreateCardTokenRequest, error) {
		consumed++
		return consumeIntentCard()
	}
	request := models.ConfirmPaymentIntentRequest{CardToken: intentCardToken}
	current, attempt, card, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "pay", "original", request, consume)
	require.ErrorIs(t, err, failure)
	assert.Nil(t, current)
	assert.Nil(t, attempt)
	assert.Nil(t, card)
	stored, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentProcessing, stored.Status)
	require.Len(t, stored.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionScheduled, stored.BankTransactions[0].Status)
	assert.Equal(t, stored.BankTransactions[0].CreatedAt, stored.BankTransactions[0].UpdatedAt)
	current, attempt, card, err = repo.CreateBankTransaction("merchant", intent.PaymentIntentID, "pay", "replay", request, consume)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal(t, stored.BankTransactions[0].BankTransactionID, attempt.BankTransactionID)
	assert.Nil(t, card, "replay must not submit another bank request")
	assert.Equal(t, 1, consumed)
	assert.Empty(t, changes.events)
}

func TestPaymentIntentPrivateMetadataAndPublicationFailures(t *testing.T) {
	repo, changes, _ := intentRepository(t)
	intent := createIntent(t, repo, "merchant", "private-intent-key")
	transaction := createIntentTransaction(t, repo, intent, "private-bank-key")
	current, err := repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.NotEmpty(t, intent.RequestFingerprint)
	assert.NotEmpty(t, transaction.RequestFingerprint)
	for _, value := range []interface{}{intent, transaction, current, changes.events[0]} {
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		for _, private := range []string{
			"private-intent-key", "private-bank-key", intent.RequestFingerprint,
			transaction.RequestFingerprint, intentCardToken, "0222405343240007", "cvv",
		} {
			assert.NotContains(t, string(encoded), private)
			assert.NotContains(t, fmt.Sprintf("%+v", value), private)
		}

	}
	failure := errors.New("event transport unavailable")
	changes.err = failure
	_, err = repo.CompleteBankTransaction("merchant", transaction.BankTransactionID, models.BankTransactionAuthorized, nil)
	assert.ErrorIs(t, err, failure)
	current, err = repo.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentAuthorized, current.Status)
	assert.Equal(t, models.BankTransactionAuthorized, current.BankTransactions[0].Status)
}
