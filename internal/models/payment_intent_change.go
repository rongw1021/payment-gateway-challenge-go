package models

import (
	"encoding/json"
	"time"
)

type PaymentIntentEventType string

const (
	PaymentIntentCreatedEvent PaymentIntentEventType = "paymentintent.created"
	PaymentIntentUpdatedEvent PaymentIntentEventType = "paymentintent.updated"
)

// PaymentIntentSnapshot intentionally excludes private metadata and transaction history.
type PaymentIntentSnapshot struct {
	Status    PaymentIntentStatus `json:"status"`
	Currency  string              `json:"currency"`
	Amount    int32               `json:"amount"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

type PaymentIntentFieldChange struct {
	FieldName     string          `json:"field_name"`
	OriginalValue json.RawMessage `json:"original_value"`
	NewValue      json.RawMessage `json:"new_value"`
}

type PaymentIntentChangeEvent struct {
	Sequence        int64                      `json:"sequence"`
	RequestID       string                     `json:"request_id,omitempty"`
	EventType       PaymentIntentEventType     `json:"event_type"`
	MerchantID      string                     `json:"merchant_id"`
	PaymentIntentID string                     `json:"payment_intent_id"`
	PaymentIntent   *PaymentIntentSnapshot     `json:"payment_intent,omitempty"`
	Changes         []PaymentIntentFieldChange `json:"changes,omitempty"`
}

func (e PaymentIntentChangeEvent) WithSequence(sequence int64) PaymentIntentChangeEvent {
	e.Sequence = sequence
	return e
}
