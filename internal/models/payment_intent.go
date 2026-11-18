package models

import (
	"errors"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/identifiers"
)

type PaymentIntentStatus string

const (
	PaymentIntentCreated             PaymentIntentStatus = "Created"
	PaymentIntentProcessing          PaymentIntentStatus = "Processing"
	PaymentIntentAuthorized          PaymentIntentStatus = "Authorized"
	PaymentIntentDeclined            PaymentIntentStatus = "Declined"
	PaymentIntentPendingVerification PaymentIntentStatus = "PendingVerification"
)

var (
	ErrPaymentIntentNotFound = errors.New("payment intent not found")
	ErrIdempotencyConflict   = errors.New("idempotency key conflicts with an earlier request")
	ErrIntentNotPayable      = errors.New("payment intent is not payable")
	ErrInvalidCardToken      = errors.New("card token is unavailable or expired")
)

type CreatePaymentIntentRequest struct {
	Currency string `json:"currency" binding:"required" enums:"GBP,USD,EUR" example:"GBP"`
	// Positive amount in the currency's minor unit.
	Amount *int32 `json:"amount" binding:"required" format:"int32" minimum:"1" maximum:"2147483647" example:"100"`
}

func (r CreatePaymentIntentRequest) Validate() map[string][]string {
	fields := make(map[string][]string)
	if r.Currency != "GBP" && r.Currency != "USD" && r.Currency != "EUR" {
		fields["currency"] = []string{"Currency must be GBP, USD or EUR."}
	}
	if r.Amount == nil || *r.Amount <= 0 {
		fields["amount"] = []string{"Amount is required and must be a 32-bit integer greater than 0."}
	}
	return fields
}

type ConfirmPaymentIntentRequest struct {
	// Use a token returned by /api/CardTokens, in lowercase hyphenated UUID format.
	CardToken string `json:"card_token" binding:"required" minLength:"36" maxLength:"36" format:"uuid" example:"01900000-0000-7000-8000-000000000001"`
}

func (r ConfirmPaymentIntentRequest) Validate() map[string][]string {
	if !identifiers.IsCanonicalUUID(r.CardToken) {
		return map[string][]string{
			"card_token": {"Card token must be a lowercase hyphenated GUID."},
		}
	}
	return nil
}

func (ConfirmPaymentIntentRequest) String() string   { return "ConfirmPaymentIntentRequest" }
func (ConfirmPaymentIntentRequest) GoString() string { return "ConfirmPaymentIntentRequest" }

type PaymentIntent struct {
	PaymentIntentID    string              `json:"payment_intent_id" format:"uuid"`
	MerchantID         string              `json:"merchant_id"`
	Status             PaymentIntentStatus `json:"status"`
	Currency           string              `json:"currency"`
	Amount             int32               `json:"amount"`
	IdempotencyKey     string              `json:"-"`
	RequestFingerprint string              `json:"-"`
	BankTransactions   []BankTransaction   `json:"bank_transactions"`
}

func (p PaymentIntent) String() string {
	return "PaymentIntent { PaymentIntentID = " + p.PaymentIntentID + ", Status = " + string(p.Status) + " }"
}
func (p PaymentIntent) GoString() string { return p.String() }

type PaymentIntentResponse struct {
	PaymentIntentID string              `json:"payment_intent_id" binding:"required" format:"uuid"`
	Status          PaymentIntentStatus `json:"status" binding:"required"`
	Currency        string              `json:"currency" binding:"required" enums:"GBP,USD,EUR" example:"GBP"`
	Amount          int32               `json:"amount" binding:"required" format:"int32" minimum:"1" maximum:"2147483647" example:"100"`
	// Complete history, oldest first; empty rather than null before the first attempt.
	BankTransactions []BankTransactionResponse `json:"bank_transactions" binding:"required"`
	RequestID        *string                   `json:"request_id" binding:"required" format:"uuid"`
}
