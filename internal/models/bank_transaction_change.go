package models

import "encoding/json"

type BankTransactionEventType string

const (
	BankTransactionCreated BankTransactionEventType = "banktransaction.created"
	BankTransactionUpdated BankTransactionEventType = "banktransaction.updated"
)

type BankTransactionFieldChange struct {
	FieldName     string          `json:"field_name"`
	OriginalValue json.RawMessage `json:"original_value"`
	NewValue      json.RawMessage `json:"new_value"`
}

type BankTransactionChangeEvent struct {
	Sequence          int64                        `json:"sequence"`
	RequestID         string                       `json:"request_id" format:"uuid"`
	EventType         BankTransactionEventType     `json:"event_type"`
	MerchantID        string                       `json:"merchant_id"`
	PaymentIntentID   string                       `json:"payment_intent_id" format:"uuid"`
	BankTransactionID string                       `json:"bank_transaction_id" format:"uuid"`
	BankTransaction   *BankTransaction             `json:"bank_transaction"`
	Changes           []BankTransactionFieldChange `json:"changes"`
}

func (e BankTransactionChangeEvent) WithSequence(sequence int64) BankTransactionChangeEvent {
	e.Sequence = sequence
	return e
}
