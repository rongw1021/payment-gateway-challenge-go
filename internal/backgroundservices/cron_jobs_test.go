package backgroundservices

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/identifiers"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func workerRepository(t *testing.T) (*repository.PaymentIntentsRepository, *changedatacapture.InMemory[models.BankTransactionChangeEvent], *atomic.Int64) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	capture := changedatacapture.NewInMemory[models.BankTransactionChangeEvent]()
	repo, err := repository.NewPaymentIntentsRepository(capture, changedatacapture.NewInMemory[models.PaymentIntentChangeEvent](), func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	require.NoError(t, err)
	return repo, capture, clock
}

func workerTransaction(t *testing.T, repo *repository.PaymentIntentsRepository) models.BankTransaction {
	t.Helper()
	key, err := identifiers.NewUUID()
	require.NoError(t, err)
	amount := int32(100)
	intent, _, err := repo.CreatePaymentIntent("merchant", key, "creation-request", models.CreatePaymentIntentRequest{Currency: "GBP", Amount: &amount})
	require.NoError(t, err)
	_, transaction, _, err := repo.CreateBankTransaction("merchant", intent.PaymentIntentID, key,
		"11111111-1111-4111-8111-111111111111",
		models.ConfirmPaymentIntentRequest{CardToken: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		func() (*models.CreateCardTokenRequest, error) {
			return &models.CreateCardTokenRequest{CardNumber: "2222405343241234", ExpiryMonth: 12, ExpiryYear: 2035, Cvv: "012"}, nil
		})
	require.NoError(t, err)
	require.NotNil(t, transaction)
	return *transaction
}

func runWorker(t *testing.T, run func(context.Context) error) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("background worker did not stop")
		}
	})
	return cancel, done
}

func TestCronJobStartupSweepHonorsBoundaryAndBatchLimit(t *testing.T) {
	repo, capture, clock := workerRepository(t)
	first := workerTransaction(t, repo)
	second := workerTransaction(t, repo)
	authorized := workerTransaction(t, repo)
	_, err := repo.CompleteBankTransaction("merchant", authorized.BankTransactionID, models.BankTransactionAuthorized, nil)
	require.NoError(t, err)
	pending := workerTransaction(t, repo)
	_, err = repo.CompleteBankTransaction("merchant", pending.BankTransactionID, models.BankTransactionServiceUnavailable, nil)
	require.NoError(t, err)
	clock.Add(int64(30 * time.Second))
	fresh := workerTransaction(t, repo)
	options := CronJobOptions{SweepInterval: time.Hour, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 1}
	service, err := NewCronJobService(repo, func() time.Time { return time.Unix(0, clock.Load()).UTC() }, options)
	require.NoError(t, err)
	cancel, done := runWorker(t, service.Run)
	require.Eventually(t, func() bool {
		transaction, err := repo.GetBankTransaction("merchant", first.BankTransactionID)
		return err == nil && transaction.Status == models.BankTransactionStatusUpdateTimeout
	}, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	remaining, err := repo.GetTimedOutBankTransactions(time.Unix(0, clock.Load()).Add(-30*time.Second), 100)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, second.BankTransactionID, remaining[0].BankTransactionID)
	for id, expected := range map[string]models.BankTransactionStatus{
		authorized.BankTransactionID: models.BankTransactionAuthorized,
		pending.BankTransactionID:    models.BankTransactionServiceUnavailable,
		fresh.BankTransactionID:      models.BankTransactionScheduled,
	} {
		transaction, err := repo.GetBankTransaction("merchant", id)
		require.NoError(t, err)
		assert.Equal(t, expected, transaction.Status)
	}
	intent, err := repo.GetPaymentIntent("merchant", first.PaymentIntentID)
	require.NoError(t, err)
	assert.Equal(t, models.PaymentIntentPendingVerification, intent.Status)
	assert.Equal(t, time.Unix(0, clock.Load()).UTC(), intent.BankTransactions[0].UpdatedAt)
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	var last models.BankTransactionChangeEvent
	for i := 0; i < 8; i++ {
		last, err = capture.Consume(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, first.BankTransactionID, last.BankTransactionID)
	assert.Equal(t, first.OriginatingRequestID, last.RequestID)
	assert.JSONEq(t, `"StatusUpdateTimeout"`, string(last.Changes[0].NewValue))
}

func TestCronJobPeriodicSweepFindsBackdatedAndEqualTimestampRecords(t *testing.T) {
	repo, _, clock := workerRepository(t)
	first := workerTransaction(t, repo)
	clock.Add(int64(30 * time.Second))
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service, err := NewCronJobService(repo, now, CronJobOptions{
		SweepInterval: 10 * time.Millisecond, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 1,
	})
	require.NoError(t, err)
	runWorker(t, service.Run)
	timedOut := func(id string) bool {
		transaction, err := repo.GetBankTransaction("merchant", id)
		return err == nil && transaction.Status == models.BankTransactionStatusUpdateTimeout
	}
	require.Eventually(t, func() bool { return timedOut(first.BankTransactionID) }, 5*time.Second, time.Millisecond)
	clock.Add(-int64(40 * time.Second))
	second := workerTransaction(t, repo)
	third := workerTransaction(t, repo)
	clock.Add(int64(40 * time.Second))
	require.Eventually(t, func() bool {
		return timedOut(second.BankTransactionID) && timedOut(third.BankTransactionID)
	}, 5*time.Second, time.Millisecond)
	remaining, err := repo.GetTimedOutBankTransactions(now().Add(-30*time.Second), 100)
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

type cronRepositoryStub struct {
	query func(time.Time, int) ([]models.BankTransaction, error)
	mark  func(string, string, time.Time) (bool, error)
}

func (r cronRepositoryStub) GetTimedOutBankTransactions(cutoff time.Time, batch int) ([]models.BankTransaction, error) {
	return r.query(cutoff, batch)
}

func (r cronRepositoryStub) TryMarkTimedOutBankTransaction(merchant, id string, cutoff time.Time) (bool, error) {
	return r.mark(merchant, id, cutoff)
}

func TestCronJobStopsWhileTimerIsIdle(t *testing.T) {
	queried := make(chan struct{})
	now := time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC)
	service, err := NewCronJobService(cronRepositoryStub{
		query: func(cutoff time.Time, batchSize int) ([]models.BankTransaction, error) {
			assert.Equal(t, now.Add(-30*time.Second), cutoff)
			assert.Equal(t, 7, batchSize)
			close(queried)
			return nil, nil
		},
	}, func() time.Time { return now }, CronJobOptions{
		SweepInterval: time.Hour, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 7,
	})
	require.NoError(t, err)
	cancel, done := runWorker(t, service.Run)
	select {
	case <-queried:
	case <-time.After(5 * time.Second):
		t.Fatal("startup sweep did not run")
	}
	cancel()
	require.NoError(t, <-done)
}

func TestCronJobCancellationStopsTheBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := 0
	service, err := NewCronJobService(cronRepositoryStub{
		query: func(time.Time, int) ([]models.BankTransaction, error) {
			return []models.BankTransaction{
				{MerchantID: "merchant", BankTransactionID: "first"},
				{MerchantID: "merchant", BankTransactionID: "second"},
			}, nil
		},
		mark: func(string, string, time.Time) (bool, error) {
			updates++
			cancel()
			return true, nil
		},
	}, time.Now, CronJobOptions{SweepInterval: time.Hour, BankTransactionStatusUpdateTimeout: time.Second, BatchSize: 2})
	require.NoError(t, err)
	require.NoError(t, service.Run(ctx))
	assert.Equal(t, 1, updates)
}

func TestCronJobPropagatesRepositoryFailures(t *testing.T) {
	failure := errors.New("repository unavailable")
	for _, duringUpdate := range []bool{false, true} {
		repo := cronRepositoryStub{
			query: func(time.Time, int) ([]models.BankTransaction, error) {
				if duringUpdate {
					return []models.BankTransaction{{MerchantID: "merchant", BankTransactionID: "transaction"}}, nil
				}
				return nil, failure
			},
			mark: func(string, string, time.Time) (bool, error) { return false, failure },
		}
		service, err := NewCronJobService(repo, time.Now, CronJobOptions{
			SweepInterval: time.Hour, BankTransactionStatusUpdateTimeout: time.Second, BatchSize: 1,
		})
		require.NoError(t, err)
		assert.ErrorIs(t, service.Run(context.Background()), failure)
	}
}

func TestCronJobConfigurationValidation(t *testing.T) {
	valid := CronJobOptions{SweepInterval: time.Second, BankTransactionStatusUpdateTimeout: 30 * time.Second, BatchSize: 100}
	for _, duration := range []time.Duration{0, -time.Millisecond, time.Millisecond - 1, (1 << 31) * time.Millisecond} {
		options := valid
		options.SweepInterval = duration
		_, err := NewCronJobService(cronRepositoryStub{}, time.Now, options)
		require.ErrorContains(t, err, "SweepInterval")
		options = valid
		options.BankTransactionStatusUpdateTimeout = duration
		_, err = NewCronJobService(cronRepositoryStub{}, time.Now, options)
		require.ErrorContains(t, err, "BankTransactionStatusUpdateTimeout")
	}
	for _, batch := range []int{0, -1} {
		options := valid
		options.BatchSize = batch
		_, err := NewCronJobService(cronRepositoryStub{}, time.Now, options)
		require.ErrorContains(t, err, "BatchSize")
	}
	for _, duration := range []time.Duration{time.Millisecond, (1<<31 - 1) * time.Millisecond} {
		options := valid
		options.SweepInterval, options.BankTransactionStatusUpdateTimeout = duration, duration
		_, err := NewCronJobService(cronRepositoryStub{}, time.Now, options)
		require.NoError(t, err)
	}
}
