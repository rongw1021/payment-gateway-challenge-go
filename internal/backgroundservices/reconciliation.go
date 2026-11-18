package backgroundservices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

type ReconciliationService struct {
	changes changedatacapture.ChangeDataCapture
	logger  *slog.Logger
}

func NewReconciliationService(changes changedatacapture.ChangeDataCapture, logger *slog.Logger) (*ReconciliationService, error) {
	if changes == nil || logger == nil {
		return nil, errors.New("change data capture and logger are required")
	}
	return &ReconciliationService{changes: changes, logger: logger}, nil
}

func (s *ReconciliationService) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		change, err := s.changes.Consume(ctx)
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil
			}
			return fmt.Errorf("consume bank transaction events: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := s.logChange(ctx, change); err != nil {
			return err
		}
	}
}

func (s *ReconciliationService) logChange(ctx context.Context, change models.BankTransactionChangeEvent) error {
	var statusChange *models.BankTransactionFieldChange
	for i := range change.Changes {
		field := &change.Changes[i]
		if statusChange == nil && field.FieldName == "Status" {
			statusChange = field
		}
	}
	var originalStatus, newStatus string
	if statusChange != nil {
		if err := json.Unmarshal(statusChange.OriginalValue, &originalStatus); err != nil {
			return fmt.Errorf("decode original bank transaction status: %w", err)
		}
		if err := json.Unmarshal(statusChange.NewValue, &newStatus); err != nil {
			return fmt.Errorf("decode new bank transaction status: %w", err)
		}
	}
	if change.BankTransaction != nil {
		newStatus = string(change.BankTransaction.Status)
	}
	logger := s.logger.With(
		"request_id", change.RequestID, "merchant_id", change.MerchantID,
		"event_type", string(change.EventType), "sequence", change.Sequence,
		"bank_transaction_id", change.BankTransactionID, "payment_intent_id", change.PaymentIntentID,
	)
	safeChange := change
	if change.BankTransaction != nil {
		transaction := *change.BankTransaction
		transaction.AuthorizationCode = nil
		safeChange.BankTransaction = &transaction
	}
	safeChange.Changes = append([]models.BankTransactionFieldChange{}, change.Changes...)
	for i := range safeChange.Changes {
		if safeChange.Changes[i].FieldName == "AuthorizationCode" {
			safeChange.Changes[i].OriginalValue = json.RawMessage(`"[REDACTED]"`)
			safeChange.Changes[i].NewValue = json.RawMessage(`"[REDACTED]"`)
		}
	}
	message, err := json.Marshal(safeChange)
	if err != nil {
		return fmt.Errorf("encode bank transaction change log: %w", err)
	}
	if change.EventType == models.BankTransactionCreated {
		logger.InfoContext(ctx, string(message), "new_status", newStatus)
	} else if statusChange != nil {
		logger.InfoContext(ctx, string(message), "original_status", originalStatus, "new_status", newStatus)
	} else {
		logger.InfoContext(ctx, string(message))
	}
	if change.EventType == models.BankTransactionUpdated && statusChange != nil &&
		models.BankTransactionStatus(newStatus).RequiresVerification() {
		logger.WarnContext(ctx,
			"The bank simulator has no verification API; no verification or payment retry was attempted.",
			"bank_transaction_status", newStatus)
	}
	return nil
}
