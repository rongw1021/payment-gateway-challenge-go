package backgroundservices

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

type CronJobOptions struct {
	SweepInterval                      time.Duration
	BankTransactionStatusUpdateTimeout time.Duration
	BatchSize                          int
}

func (o CronJobOptions) Validate() error {
	const maxDuration = (1<<31 - 1) * time.Millisecond
	if o.SweepInterval < time.Millisecond || o.SweepInterval > maxDuration {
		return errors.New("Reconciliation:SweepInterval must be between 1 and 2147483647 milliseconds")
	}
	if o.BankTransactionStatusUpdateTimeout < time.Millisecond || o.BankTransactionStatusUpdateTimeout > maxDuration {
		return errors.New("Reconciliation:BankTransactionStatusUpdateTimeout must be between 1 and 2147483647 milliseconds")
	}
	if o.BatchSize <= 0 {
		return errors.New("Reconciliation:BatchSize must be positive")
	}
	return nil
}

type TimedOutTransactionsRepository interface {
	GetTimedOutBankTransactions(time.Time, int) ([]models.BankTransaction, error)
	TryMarkTimedOutBankTransaction(string, string, time.Time) (bool, error)
}

type CronJobService struct {
	repository TimedOutTransactionsRepository
	now        func() time.Time
	options    CronJobOptions
}

func NewCronJobService(repo TimedOutTransactionsRepository, now func() time.Time, options CronJobOptions) (*CronJobService, error) {
	if repo == nil || now == nil {
		return nil, errors.New("transaction repository and clock are required")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &CronJobService{repository: repo, now: now, options: options}, nil
}

func (s *CronJobService) Run(ctx context.Context) error {
	timer := time.NewTicker(s.options.SweepInterval)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		cutoff := s.now().UTC().Add(-s.options.BankTransactionStatusUpdateTimeout)
		transactions, err := s.repository.GetTimedOutBankTransactions(cutoff, s.options.BatchSize)
		if err != nil {
			return fmt.Errorf("find timed-out bank transactions: %w", err)
		}
		for _, transaction := range transactions {
			if ctx.Err() != nil {
				return nil
			}
			if _, err := s.repository.TryMarkTimedOutBankTransaction(transaction.MerchantID, transaction.BankTransactionID, cutoff); err != nil {
				return fmt.Errorf("mark timed-out bank transaction: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
