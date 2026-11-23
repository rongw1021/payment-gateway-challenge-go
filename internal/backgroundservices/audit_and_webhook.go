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

type AuditAndWebhookService struct {
	changes changedatacapture.ChangeDataCapture[models.PaymentIntentChangeEvent]
	logger  *slog.Logger
}

func NewAuditAndWebhookService(changes changedatacapture.ChangeDataCapture[models.PaymentIntentChangeEvent], logger *slog.Logger) (*AuditAndWebhookService, error) {
	if changes == nil || logger == nil {
		return nil, errors.New("payment intent change data capture and logger are required")
	}
	return &AuditAndWebhookService{changes: changes, logger: logger}, nil
}

func (s *AuditAndWebhookService) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		event, err := s.changes.Consume(ctx)
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil
			}
			return fmt.Errorf("consume payment intent events: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := s.audit(ctx, event); err != nil {
			return err
		}
		s.webhook(ctx, event)
	}
}

func (s *AuditAndWebhookService) eventLogger(event models.PaymentIntentChangeEvent) *slog.Logger {
	logger := s.logger.With("merchant_id", event.MerchantID,
		"payment_intent_id", event.PaymentIntentID, "event_type", string(event.EventType),
		"sequence", event.Sequence)
	if event.RequestID != "" {
		logger = logger.With("request_id", event.RequestID)
	}
	return logger
}

func (s *AuditAndWebhookService) audit(ctx context.Context, event models.PaymentIntentChangeEvent) error {
	// Intent audits deliberately retain the whole safe event as the message.
	message, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("serialize payment intent audit event: %w", err)
	}
	s.eventLogger(event).InfoContext(ctx, string(message))
	return nil
}

func (s *AuditAndWebhookService) webhook(ctx context.Context, event models.PaymentIntentChangeEvent) {
	s.eventLogger(event).WarnContext(ctx, "webhook delivery is not implemented at this time")
}
