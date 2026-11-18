package models

import (
	"fmt"
	"time"
)

type BankTransactionStatus string

const (
	BankTransactionScheduled           BankTransactionStatus = "Scheduled"
	BankTransactionAuthorized          BankTransactionStatus = "Authorized"
	BankTransactionDeclined            BankTransactionStatus = "Declined"
	BankTransactionRequestTimeout      BankTransactionStatus = "RequestTimeout"
	BankTransactionStatusUpdateTimeout BankTransactionStatus = "StatusUpdateTimeout"
	BankTransactionServiceUnavailable  BankTransactionStatus = "ServiceUnavailable"
	BankTransactionNetworkError        BankTransactionStatus = "NetworkError"
	BankTransactionInvalidResponse     BankTransactionStatus = "InvalidResponse"
	BankTransactionRequestCanceled     BankTransactionStatus = "RequestCanceled"
)

func (s BankTransactionStatus) RequiresVerification() bool {
	switch s {
	case BankTransactionRequestTimeout, BankTransactionStatusUpdateTimeout,
		BankTransactionServiceUnavailable, BankTransactionNetworkError,
		BankTransactionInvalidResponse, BankTransactionRequestCanceled:
		return true
	default:
		return false
	}
}

func (s BankTransactionStatus) ToPaymentIntentStatus() (PaymentIntentStatus, error) {
	switch s {
	case BankTransactionScheduled:
		return PaymentIntentProcessing, nil
	case BankTransactionAuthorized:
		return PaymentIntentAuthorized, nil
	case BankTransactionDeclined:
		return PaymentIntentDeclined, nil
	default:
		if s.RequiresVerification() {
			return PaymentIntentPendingVerification, nil
		}
		return "", fmt.Errorf("unsupported bank transaction status %q", s)
	}
}

type BankTransaction struct {
	BankTransactionID    string                `json:"bank_transaction_id" format:"uuid"`
	PaymentIntentID      string                `json:"payment_intent_id" format:"uuid"`
	MerchantID           string                `json:"merchant_id"`
	Status               BankTransactionStatus `json:"status"`
	CardNumberLastFour   string                `json:"card_number_last_four"`
	ExpiryMonth          int                   `json:"expiry_month"`
	ExpiryYear           int                   `json:"expiry_year"`
	CreatedAt            time.Time             `json:"created_at"`
	IdempotencyKey       string                `json:"-"`
	RequestFingerprint   string                `json:"-"`
	OriginatingRequestID string                `json:"-"`
	CompletedAt          *time.Time            `json:"completed_at"`
	AuthorizationCode    *string               `json:"authorization_code"`
}

func (b BankTransaction) String() string {
	return "BankTransaction { BankTransactionID = " + b.BankTransactionID + ", Status = " + string(b.Status) + " }"
}
func (b BankTransaction) GoString() string { return b.String() }

type BankTransactionResponse struct {
	BankTransactionID  string                `json:"bank_transaction_id" binding:"required" format:"uuid"`
	Status             BankTransactionStatus `json:"status" binding:"required"`
	CardNumberLastFour string                `json:"card_number_last_four" binding:"required" minLength:"4" maxLength:"4" example:"0007"`
	ExpiryMonth        int                   `json:"expiry_month" binding:"required" minimum:"1" maximum:"12" example:"4"`
	ExpiryYear         int                   `json:"expiry_year" binding:"required" minimum:"1" maximum:"9999" example:"2099"`
	CreatedAt          time.Time             `json:"created_at" binding:"required" format:"date-time"`
	// Null while Scheduled, including an in-flight bank request.
	CompletedAt *time.Time `json:"completed_at" binding:"required" format:"date-time" extensions:"x-nullable"`
}
