package models

import "time"

type CreateCardTokenRequest struct {
	// Between 14 and 19 ASCII digits; the example is a synthetic simulator card.
	CardNumber string `json:"card_number" binding:"required" minLength:"14" maxLength:"19" example:"0222405343240007"`
	// The combined month/year must not be in the past; the current month is accepted.
	ExpiryMonth int `json:"expiry_month" binding:"required" minimum:"1" maximum:"12" example:"4"`
	ExpiryYear  int `json:"expiry_year" binding:"required" minimum:"1" maximum:"9999" example:"2099"`
	// Three or four ASCII digits.
	Cvv string `json:"cvv" binding:"required" minLength:"3" maxLength:"4" example:"012"`
}

func (r CreateCardTokenRequest) Validate() map[string][]string {
	fields := make(map[string][]string)
	if !digitsBetween(r.CardNumber, 14, 19) {
		fields["card_number"] = []string{"Card number must contain 14 to 19 ASCII digits."}
	}
	if r.ExpiryMonth < 1 || r.ExpiryMonth > 12 {
		fields["expiry_month"] = []string{"Expiry month must be between 1 and 12."}
	}
	if r.ExpiryYear < 1 || r.ExpiryYear > 9999 {
		fields["expiry_year"] = []string{"Expiry year must be between 1 and 9999."}
	}
	if !digitsBetween(r.Cvv, 3, 4) {
		fields["cvv"] = []string{"CVV must contain 3 or 4 ASCII digits."}
	}
	return fields
}

func (CreateCardTokenRequest) String() string   { return "CreateCardTokenRequest" }
func (CreateCardTokenRequest) GoString() string { return "CreateCardTokenRequest" }

type CardTokenResponse struct {
	// Merchant-owned, single-use, canonical UUIDv7.
	Token     string    `json:"token" binding:"required" format:"uuid" example:"01900000-0000-7000-8000-000000000001"`
	ExpiresAt time.Time `json:"expires_at" binding:"required" format:"date-time"`
	RequestID string    `json:"request_id" binding:"required" format:"uuid"`
}

func (CardTokenResponse) String() string   { return "CardTokenResponse" }
func (CardTokenResponse) GoString() string { return "CardTokenResponse" }

type ProblemDetails struct {
	Title                 string                `json:"title" binding:"required"`
	Status                int                   `json:"status" binding:"required" minimum:"400" maximum:"599"`
	Detail                string                `json:"detail,omitempty"`
	RequestID             string                `json:"request_id" binding:"required" format:"uuid"`
	Errors                map[string][]string   `json:"errors,omitempty"`
	PaymentIntentID       string                `json:"payment_intent_id,omitempty" format:"uuid"`
	BankTransactionID     string                `json:"bank_transaction_id,omitempty" format:"uuid"`
	PaymentIntentStatus   PaymentIntentStatus   `json:"payment_intent_status,omitempty"`
	BankTransactionStatus BankTransactionStatus `json:"bank_transaction_status,omitempty"`
}

func digitsBetween(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
