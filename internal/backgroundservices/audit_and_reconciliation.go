package backgroundservices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/changedatacapture"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

type AuditAndReconciliationService struct {
	changes changedatacapture.ChangeDataCapture[models.BankTransactionChangeEvent]
	logger  *slog.Logger
}

func NewAuditAndReconciliationService(changes changedatacapture.ChangeDataCapture[models.BankTransactionChangeEvent], logger *slog.Logger) (*AuditAndReconciliationService, error) {
	if changes == nil || logger == nil {
		return nil, errors.New("change data capture and logger are required")
	}
	return &AuditAndReconciliationService{changes: changes, logger: logger}, nil
}

func (s *AuditAndReconciliationService) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		event, err := s.changes.Consume(ctx)
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil
			}
			return fmt.Errorf("consume bank transaction events: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := s.audit(ctx, event); err != nil {
			return err
		}
		if err := s.reconcile(ctx, event); err != nil {
			return err
		}
	}
}

func (s *AuditAndReconciliationService) eventLogger(event models.BankTransactionChangeEvent) *slog.Logger {
	logger := s.logger.With("event_type", string(event.EventType), "sequence", event.Sequence)
	for _, field := range []struct{ name, value string }{
		{"request_id", event.RequestID}, {"merchant_id", event.MerchantID},
		{"payment_intent_id", event.PaymentIntentID}, {"bank_transaction_id", event.BankTransactionID},
	} {
		if field.value != "" {
			logger = logger.With(field.name, field.value)
		}
	}
	return logger
}

func (s *AuditAndReconciliationService) audit(ctx context.Context, event models.BankTransactionChangeEvent) error {
	// Bank audits retain the whole event, but redact sensitive values on a detached copy.
	safeEvent := event
	if event.BankTransaction != nil {
		transaction := *event.BankTransaction
		transaction.AuthorizationCode = nil
		safeEvent.BankTransaction = &transaction
	}
	safeEvent.Changes = slices.Clone(event.Changes)
	for i := range safeEvent.Changes {
		field := &safeEvent.Changes[i]
		if field.FieldName != "Status" && field.FieldName != "UpdatedAt" {
			field.OriginalValue = json.RawMessage(`"[REDACTED]"`)
			field.NewValue = json.RawMessage(`"[REDACTED]"`)
		}
	}
	message, err := json.Marshal(safeEvent)
	if err != nil {
		return fmt.Errorf("serialize bank transaction audit event: %w", err)
	}
	s.eventLogger(event).InfoContext(ctx, string(message))
	return nil
}

func (s *AuditAndReconciliationService) reconcile(ctx context.Context, event models.BankTransactionChangeEvent) error {
	if event.EventType != models.BankTransactionUpdated {
		return nil
	}
	for _, field := range event.Changes {
		if field.FieldName != "Status" {
			continue
		}
		var status models.BankTransactionStatus
		if err := json.Unmarshal(field.NewValue, &status); err != nil {
			return fmt.Errorf("decode new bank transaction status: %w", err)
		}
		if status.RequiresVerification() {
			s.eventLogger(event).WarnContext(ctx,
				"The bank simulator has no verification API; no verification or payment retry was attempted.",
				"bank_transaction_status", string(status))
		}
		break
	}
	return nil
}
