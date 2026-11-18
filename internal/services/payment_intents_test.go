package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	paymentRequestID      = "11111111-1111-4111-8111-111111111111"
	paymentRetryRequestID = "22222222-2222-4222-8222-222222222222"
)

type paymentBank struct {
	calls atomic.Int32
	send  func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error)
}

func (b *paymentBank) ProcessBankTransaction(ctx context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
	b.calls.Add(1)
	return b.send(ctx, request)
}

type paymentEvents struct {
	mu     sync.Mutex
	events []models.BankTransactionChangeEvent
}

func (e *paymentEvents) Produce(event models.BankTransactionChangeEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	return nil
}

type paymentFixture struct {
	service *PaymentIntentsService
	repo    *repository.PaymentIntentsRepository
	cards   *CardTokenService
	bank    *paymentBank
	clock   *atomic.Int64
	events  *paymentEvents
}

func newPaymentFixture(t *testing.T, send func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error)) *paymentFixture {
	t.Helper()
	cards, _, clock := cardService(t)
	if send == nil {
		send = func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
			code := "bank-code"
			return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: &code}, nil
		}
	}
	bank := &paymentBank{send: send}
	events := &paymentEvents{}
	repo, err := repository.NewPaymentIntentsRepository(events, cards.now)
	require.NoError(t, err)
	service, err := NewPaymentIntentsService(repo, bank, cards, cards.now)
	require.NoError(t, err)
	return &paymentFixture{service: service, repo: repo, cards: cards, bank: bank, clock: clock, events: events}
}

func (f *paymentFixture) create(t *testing.T, merchant, key string) *models.PaymentIntentResponse {
	t.Helper()
	amount := int32(100)
	intent, conflict, err := f.service.CreatePaymentIntent(context.Background(), merchant, key,
		models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	require.False(t, conflict)
	return intent
}

func (f *paymentFixture) token(t *testing.T, merchant string, card models.CreateCardTokenRequest) string {
	t.Helper()
	token, err := f.cards.Issue(context.Background(), merchant, card)
	require.NoError(t, err)
	return token.Token
}

func (f *paymentFixture) confirm(merchant, intent, key, token string) (*models.PaymentIntentResponse, error) {
	return f.service.ConfirmPaymentIntent(middleware.WithRequestID(context.Background(), paymentRequestID), merchant, intent, key,
		models.ConfirmPaymentIntentRequest{CardToken: token})
}

func TestPaymentIntentConfirmationUsesStoredDetailsAndReplaysAfterTokenExpiry(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(fmt.Sprint(authorized), func(t *testing.T) {
			f := newPaymentFixture(t, func(_ context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
				assert.Equal(t, "0222405343240007", request.CardNumber)
				assert.Equal(t, "04/2035", request.ExpiryDate)
				assert.Equal(t, "012", request.Cvv)
				assert.Equal(t, "GBP", request.Currency)
				assert.Equal(t, int32(100), request.Amount)
				assert.Equal(t, "BankTransactionRequest", fmt.Sprintf("%+v", request))
				code := "bank-code"
				return banksimulator.BankAuthorizationResponse{Authorized: authorized, AuthorizationCode: &code}, nil
			})
			intent := f.create(t, "merchant", "private-create")
			assert.Empty(t, intent.BankTransactions)
			assert.Zero(t, f.bank.calls.Load())
			token := f.token(t, "merchant", validCard())
			confirmed, err := f.confirm("merchant", intent.PaymentIntentID, "private-pay", token)
			require.NoError(t, err)
			expected := models.BankTransactionDeclined
			if authorized {
				expected = models.BankTransactionAuthorized
			}
			require.Len(t, confirmed.BankTransactions, 1)
			assert.Equal(t, expected, confirmed.BankTransactions[0].Status)
			assert.Equal(t, "0007", confirmed.BankTransactions[0].CardNumberLastFour)
			f.clock.Add(int64(16 * time.Minute))
			replay, err := f.service.ConfirmPaymentIntent(middleware.WithRequestID(context.Background(), paymentRetryRequestID), "merchant", intent.PaymentIntentID,
				"private-pay", models.ConfirmPaymentIntentRequest{CardToken: token})
			require.NoError(t, err)
			assert.Equal(t, confirmed, replay)
			assert.EqualValues(t, 1, f.bank.calls.Load())
			current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			assert.Equal(t, confirmed, current)
			assert.Equal(t, current, f.create(t, "merchant", "private-create"))
			require.Len(t, current.BankTransactions, 1)
			assert.NotNil(t, current.BankTransactions[0].CompletedAt)
			for _, value := range []interface{}{current, confirmed} {
				body, err := json.Marshal(value)
				require.NoError(t, err)
				for _, private := range []string{"merchant", "private-create", "private-pay", "bank-code", "cvv", token, validCard().CardNumber} {
					assert.NotContains(t, string(body), private)
				}
			}
			body, err := json.Marshal(current)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &fields))
			assert.Len(t, fields, 6)
			for _, key := range []string{"payment_intent_id", "status", "currency", "amount", "bank_transactions", "request_id"} {
				assert.Contains(t, fields, key)
			}
			require.Len(t, f.events.events, 2)
			for _, event := range f.events.events {
				assert.Equal(t, paymentRequestID, event.RequestID)
				assert.Equal(t, confirmed.BankTransactions[0].BankTransactionID, event.BankTransactionID)
			}
			for _, lookup := range []struct{ merchant, id string }{{"other", intent.PaymentIntentID}, {"merchant", "missing"}} {
				missing, err := f.service.GetPaymentIntent(lookup.merchant, lookup.id)
				require.NoError(t, err)
				assert.Nil(t, missing)
			}
		})
	}
}

func TestPaymentIntentDeclinedRetryAndOldAttemptReplay(t *testing.T) {
	f := newPaymentFixture(t, func(_ context.Context, request banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		code := "bank-code"
		return banksimulator.BankAuthorizationResponse{Authorized: strings.HasSuffix(request.CardNumber, "7"), AuthorizationCode: &code}, nil
	})
	intent := f.create(t, "merchant", "create")
	card := validCard()
	card.CardNumber = "2222405343240002"
	firstToken := f.token(t, "merchant", card)
	first, err := f.confirm("merchant", intent.PaymentIntentID, "first", firstToken)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentDeclined, first.Status)
	require.Len(t, first.BankTransactions, 1)
	secondToken := f.token(t, "merchant", validCard())
	_, err = f.confirm("merchant", intent.PaymentIntentID, "first", secondToken)
	assert.ErrorIs(t, err, models.ErrIdempotencyConflict)
	_, err = f.confirm("merchant", intent.PaymentIntentID, "second", firstToken)
	assert.ErrorIs(t, err, models.ErrInvalidCardToken)
	current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentDeclined, current.Status)
	f.clock.Add(int64(time.Minute))
	second, err := f.confirm("merchant", intent.PaymentIntentID, "second", secondToken)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentAuthorized, second.Status)
	require.Len(t, second.BankTransactions, 2)
	replay, err := f.confirm("merchant", intent.PaymentIntentID, "first", firstToken)
	require.NoError(t, err)
	assert.Equal(t, second, replay)
	assert.Equal(t, models.PaymentIntentDeclined, first.Status)
	assert.Len(t, first.BankTransactions, 1)
	current, err = f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentAuthorized, current.Status)
	require.Len(t, current.BankTransactions, 2)
	assert.Equal(t, first.BankTransactions[0].BankTransactionID, current.BankTransactions[0].BankTransactionID)
	assert.Equal(t, second.BankTransactions[1].BankTransactionID, current.BankTransactions[1].BankTransactionID)
	assert.Equal(t, current, replay)
	unusedToken := f.token(t, "merchant", validCard())
	_, err = f.confirm("merchant", intent.PaymentIntentID, "third", unusedToken)
	assert.ErrorIs(t, err, models.ErrIntentNotPayable)
	unused, err := f.cards.TryConsume(context.Background(), "merchant", unusedToken)
	require.NoError(t, err)
	assert.NotNil(t, unused)
	assert.EqualValues(t, 2, f.bank.calls.Load())
}

func TestPaymentIntentValidatedInputsPreserveAmountsAndDetectConflicts(t *testing.T) {
	f := newPaymentFixture(t, nil)
	intent := f.create(t, "merchant", "create")
	for _, amount := range []int32{1, 2147483647} {
		created, _, err := f.service.CreatePaymentIntent(context.Background(), "merchant", fmt.Sprint(amount),
			models.CreatePaymentIntentRequest{Currency: "USD", Amount: &amount})
		require.NoError(t, err)
		assert.Equal(t, amount, created.Amount)
	}
	changed := int32(101)
	_, conflict, err := f.service.CreatePaymentIntent(context.Background(), "merchant", "create",
		models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &changed})
	require.NoError(t, err)
	assert.True(t, conflict)
	assert.Zero(t, f.bank.calls.Load())
	_, err = f.confirm("merchant", intent.PaymentIntentID, "pay", f.token(t, "merchant", validCard()))
	require.NoError(t, err)
}

func TestPaymentIntentInvalidOrForeignTokensLeaveKeyAvailable(t *testing.T) {
	for _, scenario := range []string{"unknown", "expired", "foreign", "card-expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPaymentFixture(t, nil)
			card := validCard()
			if scenario == "card-expired" {
				f.clock.Store(time.Date(2030, 1, 31, 23, 59, 0, 0, time.UTC).UnixNano())
				card.ExpiryMonth, card.ExpiryYear = 1, 2030
			}
			intent := f.create(t, "merchant", "create")
			owner := "merchant"
			if scenario == "foreign" {
				owner = "other"
			}
			token := f.token(t, owner, card)
			switch scenario {
			case "unknown":
				token = "ffffffff-ffff-ffff-ffff-ffffffffffff"
			case "expired":
				f.clock.Add(int64(15 * time.Minute))
			case "card-expired":
				f.clock.Add(int64(2 * time.Minute))
			}
			_, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
			assert.ErrorIs(t, err, models.ErrInvalidCardToken)
			current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			assert.Equal(t, models.PaymentIntentCreated, current.Status)
			assert.Empty(t, current.BankTransactions)
			assert.Zero(t, f.bank.calls.Load())
			if scenario == "foreign" {
				consumed, err := f.cards.TryConsume(context.Background(), "other", token)
				require.NoError(t, err)
				assert.NotNil(t, consumed)
			}
			_, err = f.confirm("merchant", intent.PaymentIntentID, "pay", f.token(t, "merchant", validCard()))
			require.NoError(t, err)
		})
	}
}

func TestPaymentIntentMerchantsCanShareKeysButNotIntents(t *testing.T) {
	f := newPaymentFixture(t, nil)
	first := f.create(t, "one", "create")
	second := f.create(t, "two", "create")
	firstToken := f.token(t, "one", validCard())
	secondToken := f.token(t, "two", validCard())
	_, err := f.confirm("one", second.PaymentIntentID, "pay", firstToken)
	assert.ErrorIs(t, err, models.ErrPaymentIntentNotFound)
	_, err = f.confirm("one", first.PaymentIntentID, "pay", firstToken)
	require.NoError(t, err)
	_, err = f.confirm("two", second.PaymentIntentID, "pay", secondToken)
	require.NoError(t, err)
	assert.EqualValues(t, 2, f.bank.calls.Load())
}

func TestPaymentIntentConcurrentConfirmationsDoNotHoldRepositoryLockDuringBankIO(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newPaymentFixture(t, func(ctx context.Context, _ banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		close(entered)
		select {
		case <-release:
			return banksimulator.BankAuthorizationResponse{}, nil
		case <-ctx.Done():
			return banksimulator.BankAuthorizationResponse{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	intent := f.create(t, "merchant", "create")
	token := f.token(t, "merchant", validCard())
	finished := make(chan error, 1)
	go func() {
		_, err := f.service.ConfirmPaymentIntent(middleware.WithRequestID(ctx, paymentRequestID), "merchant", intent.PaymentIntentID, "pay",
			models.ConfirmPaymentIntentRequest{CardToken: token})
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("bank call did not start")
	}
	otherService, err := NewPaymentIntentsService(f.repo, f.bank, f.cards, f.cards.now)
	require.NoError(t, err)
	replay, err := otherService.ConfirmPaymentIntent(middleware.WithRequestID(ctx, paymentRetryRequestID), "merchant", intent.PaymentIntentID, "pay",
		models.ConfirmPaymentIntentRequest{CardToken: token})
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentProcessing, replay.Status)
	require.Len(t, replay.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionScheduled, replay.BankTransactions[0].Status)
	secondToken := f.token(t, "merchant", validCard())
	_, err = f.confirm("merchant", intent.PaymentIntentID, "different-key", secondToken)
	assert.ErrorIs(t, err, models.ErrIntentNotPayable)
	unused, err := f.cards.TryConsume(context.Background(), "merchant", secondToken)
	require.NoError(t, err)
	assert.NotNil(t, unused)
	close(release)
	require.NoError(t, <-finished)
	assert.EqualValues(t, 1, f.bank.calls.Load())
}

func TestPaymentIntentConcurrentIntentsCannotShareSingleUseToken(t *testing.T) {
	f := newPaymentFixture(t, nil)
	first, second := f.create(t, "merchant", "first"), f.create(t, "merchant", "second")
	token := f.token(t, "merchant", validCard())
	errs := make(chan error, 2)
	for _, intent := range []*models.PaymentIntentResponse{first, second} {
		go func(id string) {
			_, err := f.confirm("merchant", id, id, token)
			errs <- err
		}(intent.PaymentIntentID)
	}
	results := []error{<-errs, <-errs}
	if results[0] != nil {
		results[0], results[1] = results[1], results[0]
	}
	assert.NoError(t, results[0])
	assert.ErrorIs(t, results[1], models.ErrInvalidCardToken)
	assert.EqualValues(t, 1, f.bank.calls.Load())
}

func TestPaymentIntentBankFailuresRecordCauseAndPreventResubmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status models.BankTransactionStatus
	}{
		{"503", &banksimulator.Failure{Kind: banksimulator.HTTPFailure, StatusCode: 503}, models.BankTransactionServiceUnavailable},
		{"500", &banksimulator.Failure{Kind: banksimulator.HTTPFailure, StatusCode: 500}, models.BankTransactionInvalidResponse},
		{"429", &banksimulator.Failure{Kind: banksimulator.HTTPFailure, StatusCode: 429}, models.BankTransactionInvalidResponse},
		{"network", &banksimulator.Failure{Kind: banksimulator.NetworkFailure}, models.BankTransactionNetworkError},
		{"transport", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, models.BankTransactionNetworkError},
		{"stream", io.ErrUnexpectedEOF, models.BankTransactionNetworkError},
		{"json", &json.SyntaxError{}, models.BankTransactionInvalidResponse},
		{"response-limit", &banksimulator.Failure{Kind: banksimulator.InvalidResponseFailure}, models.BankTransactionInvalidResponse},
		{"bank-timeout", &banksimulator.Failure{Kind: banksimulator.TimeoutFailure}, models.BankTransactionRequestTimeout},
		{"context-timeout", context.DeadlineExceeded, models.BankTransactionRequestTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPaymentFixture(t, func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
				return banksimulator.BankAuthorizationResponse{}, fmt.Errorf("wrapped bank failure: %w", test.err)
			})
			intent := f.create(t, "merchant", "create")
			token := f.token(t, "merchant", validCard())
			result, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
			require.NoError(t, err)
			assert.Equal(t, models.PaymentIntentPendingVerification, result.Status)
			require.Len(t, result.BankTransactions, 1)
			assert.Equal(t, test.status, result.BankTransactions[0].Status)
			replay, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
			require.NoError(t, err)
			assert.Equal(t, result, replay)
			_, err = f.confirm("merchant", intent.PaymentIntentID, "another", f.token(t, "merchant", validCard()))
			assert.ErrorIs(t, err, models.ErrIntentNotPayable)
			current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
			require.NoError(t, err)
			assert.Equal(t, result, current)
			assert.NotNil(t, current.BankTransactions[0].CompletedAt)
			assert.EqualValues(t, 1, f.bank.calls.Load())
			assert.Len(t, f.events.events, 2)
		})
	}
}

func TestPaymentIntentInvalidDecisionAndUnexpectedBankError(t *testing.T) {
	for _, code := range []*string{nil, new(string)} {
		f := newPaymentFixture(t, func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
			return banksimulator.BankAuthorizationResponse{Authorized: true, AuthorizationCode: code}, nil
		})
		intent := f.create(t, "merchant", "create")
		result, err := f.confirm("merchant", intent.PaymentIntentID, "pay", f.token(t, "merchant", validCard()))
		require.NoError(t, err)
		require.Len(t, result.BankTransactions, 1)
		assert.Equal(t, models.BankTransactionInvalidResponse, result.BankTransactions[0].Status)
	}
	failure := errors.New("unexpected bank client error")
	f := newPaymentFixture(t, func(context.Context, banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		return banksimulator.BankAuthorizationResponse{}, failure
	})
	intent := f.create(t, "merchant", "create")
	_, err := f.confirm("merchant", intent.PaymentIntentID, "pay", f.token(t, "merchant", validCard()))
	assert.ErrorIs(t, err, failure)
	current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.BankTransactionScheduled, current.BankTransactions[0].Status)
	assert.Nil(t, current.BankTransactions[0].CompletedAt)
}

func TestPaymentIntentCancellationBeforeConfirmationPreservesToken(t *testing.T) {
	f := newPaymentFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	amount := int32(100)
	_, _, err := f.service.CreatePaymentIntent(ctx, "merchant", "create", models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	assert.ErrorIs(t, err, context.Canceled)
	intent := f.create(t, "merchant", "create")
	token := f.token(t, "merchant", validCard())
	_, err = f.service.ConfirmPaymentIntent(middleware.WithRequestID(ctx, paymentRequestID), "merchant", intent.PaymentIntentID, "pay",
		models.ConfirmPaymentIntentRequest{CardToken: token})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, f.bank.calls.Load())
	assert.Empty(t, f.events.events)
	card, err := f.cards.TryConsume(context.Background(), "merchant", token)
	require.NoError(t, err)
	assert.NotNil(t, card)
}

func TestPaymentIntentCallerCancellationIsRecordedBeforePropagation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newPaymentFixture(t, func(ctx context.Context, _ banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		cancel()
		return banksimulator.BankAuthorizationResponse{}, ctx.Err()
	})
	intent := f.create(t, "merchant", "create")
	token := f.token(t, "merchant", validCard())
	result, err := f.service.ConfirmPaymentIntent(middleware.WithRequestID(ctx, paymentRequestID), "merchant", intent.PaymentIntentID, "pay",
		models.ConfirmPaymentIntentRequest{CardToken: token})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
	replay, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
	require.NoError(t, err)
	require.Len(t, replay.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionRequestCanceled, replay.BankTransactions[0].Status)
	assert.Equal(t, models.PaymentIntentPendingVerification, replay.Status)
	assert.Len(t, f.events.events, 2)
	assert.EqualValues(t, 1, f.bank.calls.Load())
}

func TestPaymentIntentLateBankFailureRefinesTimeoutWithoutNewAttempt(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newPaymentFixture(t, func(ctx context.Context, _ banksimulator.BankTransactionRequest) (banksimulator.BankAuthorizationResponse, error) {
		close(entered)
		select {
		case <-release:
			return banksimulator.BankAuthorizationResponse{}, context.DeadlineExceeded
		case <-ctx.Done():
			return banksimulator.BankAuthorizationResponse{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	intent := f.create(t, "merchant", "create")
	token := f.token(t, "merchant", validCard())
	type outcome struct {
		response *models.PaymentIntentResponse
		err      error
	}
	finished := make(chan outcome, 1)
	go func() {
		response, err := f.service.ConfirmPaymentIntent(middleware.WithRequestID(ctx, paymentRequestID), "merchant", intent.PaymentIntentID, "pay",
			models.ConfirmPaymentIntentRequest{CardToken: token})
		finished <- outcome{response, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("bank call did not start")
	}
	current, err := f.service.GetPaymentIntent("merchant", intent.PaymentIntentID)
	require.NoError(t, err)
	transaction := current.BankTransactions[0]
	f.clock.Add(int64(30 * time.Second))
	marked, err := f.repo.TryMarkTimedOutBankTransaction("merchant", transaction.BankTransactionID, transaction.CreatedAt)
	require.NoError(t, err)
	assert.True(t, marked)
	replay, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
	require.NoError(t, err)
	require.Len(t, replay.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionStatusUpdateTimeout, replay.BankTransactions[0].Status)
	close(release)
	result := <-finished
	require.NoError(t, result.err)
	require.Len(t, result.response.BankTransactions, 1)
	assert.Equal(t, models.BankTransactionRequestTimeout, result.response.BankTransactions[0].Status)
	assert.Equal(t, transaction.BankTransactionID, result.response.BankTransactions[0].BankTransactionID)
	assert.Equal(t, models.BankTransactionStatusUpdateTimeout, replay.BankTransactions[0].Status)
	assert.EqualValues(t, 1, f.bank.calls.Load())
}

func TestPaymentIntentUsesHTTPBankClientAndReplaysWithoutResubmitting(t *testing.T) {
	for _, test := range []struct {
		name       string
		httpStatus int
		body       string
		status     models.BankTransactionStatus
	}{
		{"authorized", 200, `{"authorized":true,"authorization_code":"bank-code"}`, models.BankTransactionAuthorized},
		{"declined", 200, `{"authorized":false}`, models.BankTransactionDeclined},
		{"unavailable", 503, `{}`, models.BankTransactionServiceUnavailable},
		{"invalid HTTP status", 500, `{}`, models.BankTransactionInvalidResponse},
		{"invalid decision", 200, `{}`, models.BankTransactionInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/payments", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.httpStatus)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := banksimulator.NewClient(server.URL+"/", time.Second)
			require.NoError(t, err)
			f := newPaymentFixture(t, nil)
			f.service, err = NewPaymentIntentsService(f.repo, client, f.cards, f.cards.now)
			require.NoError(t, err)
			intent := f.create(t, "merchant", "create")
			token := f.token(t, "merchant", validCard())

			confirmed, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
			require.NoError(t, err)
			require.Len(t, confirmed.BankTransactions, 1)
			assert.Equal(t, test.status, confirmed.BankTransactions[0].Status)
			replay, err := f.confirm("merchant", intent.PaymentIntentID, "pay", token)
			require.NoError(t, err)
			assert.Equal(t, confirmed, replay)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}
