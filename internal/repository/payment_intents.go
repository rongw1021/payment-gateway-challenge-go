package repository

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/identifiers"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

var ErrBankTransactionNotFound = errors.New("bank transaction not found")

type BankTransactionChangeProducer interface {
	Produce(models.BankTransactionChangeEvent) error
}

type PaymentIntentChangeProducer interface {
	Produce(models.PaymentIntentChangeEvent) error
}

type PaymentIntentsRepository struct {
	mu                    sync.Mutex
	paymentIntents        []models.PaymentIntent
	bankTransactions      []models.BankTransaction
	fingerprintKey        [32]byte
	bankTransactionChange BankTransactionChangeProducer
	paymentIntentChange   PaymentIntentChangeProducer
	now                   func() time.Time
}

func NewPaymentIntentsRepository(bankTransactionChange BankTransactionChangeProducer, paymentIntentChange PaymentIntentChangeProducer, now func() time.Time) (*PaymentIntentsRepository, error) {
	if bankTransactionChange == nil || paymentIntentChange == nil || now == nil {
		return nil, errors.New("bank transaction and payment intent change producers and clock are required")
	}
	r := &PaymentIntentsRepository{bankTransactionChange: bankTransactionChange, paymentIntentChange: paymentIntentChange, now: now}
	if _, err := rand.Read(r.fingerprintKey[:]); err != nil {
		return nil, fmt.Errorf("generate payment request fingerprint key: %w", err)
	}
	return r, nil
}

func (r *PaymentIntentsRepository) CreatePaymentIntent(merchantID, idempotencyKey, requestID string, request models.CreatePaymentIntentRequest) (models.PaymentIntent, bool, error) {
	fingerprint, err := r.fingerprint(request)
	if err != nil {
		return models.PaymentIntent{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, intent := range r.paymentIntents {
		if intent.MerchantID == merchantID && intent.IdempotencyKey == idempotencyKey {
			hasConflict := !hmac.Equal([]byte(intent.RequestFingerprint), []byte(fingerprint))
			return r.withBankTransactions(intent), hasConflict, nil
		}
	}
	id, err := identifiers.NewUUID()
	if err != nil {
		return models.PaymentIntent{}, false, err
	}
	now := r.now().UTC()
	intent := models.PaymentIntent{
		PaymentIntentID: id, MerchantID: merchantID, Status: models.PaymentIntentCreated,
		Currency: request.Currency, Amount: *request.Amount,
		IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint,
		CreatedAt: now, UpdatedAt: now,
	}
	r.paymentIntents = append(r.paymentIntents, intent)
	if err := r.publishIntentChange(models.PaymentIntent{}, intent, requestID); err != nil {
		return models.PaymentIntent{}, false, err
	}
	return r.withBankTransactions(intent), false, nil
}

func (r *PaymentIntentsRepository) CreateBankTransaction(
	merchantID, paymentIntentID, idempotencyKey, originatingRequestID string,
	request models.ConfirmPaymentIntentRequest,
	consumeCard func() (*models.CreateCardTokenRequest, error),
) (*models.PaymentIntent, *models.BankTransaction, *models.CreateCardTokenRequest, error) {
	if consumeCard == nil {
		return nil, nil, nil, errors.New("card consumer is required")
	}
	fingerprint, err := r.fingerprint(struct {
		PaymentIntentID string `json:"payment_intent_id"`
		CardToken       string `json:"card_token"`
	}{paymentIntentID, request.CardToken})
	if err != nil {
		return nil, nil, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.findPaymentIntent(merchantID, paymentIntentID)
	if index < 0 {
		return nil, nil, nil, models.ErrPaymentIntentNotFound
	}
	intent := r.paymentIntents[index]
	originalIntent := intent
	intentSnapshot := r.withBankTransactions(intent)
	for _, transaction := range r.bankTransactions {
		if transaction.MerchantID == merchantID && transaction.IdempotencyKey == idempotencyKey {
			if !hmac.Equal([]byte(transaction.RequestFingerprint), []byte(fingerprint)) {
				return &intentSnapshot, nil, nil, models.ErrIdempotencyConflict
			}
			transactionSnapshot := cloneBankTransaction(transaction)
			return &intentSnapshot, &transactionSnapshot, nil, nil
		}
	}
	if intent.Status != models.PaymentIntentCreated && intent.Status != models.PaymentIntentDeclined {
		return &intentSnapshot, nil, nil, models.ErrIntentNotPayable
	}
	card, err := consumeCard()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("consume card token: %w", err)
	}
	if card == nil {
		return &intentSnapshot, nil, nil, models.ErrInvalidCardToken
	}
	id, err := identifiers.NewUUID()
	if err != nil {
		return nil, nil, nil, err
	}
	// Detach the suffix so stored records do not retain the full PAN's backing string.
	lastFour := strings.Clone(card.CardNumber[len(card.CardNumber)-4:])
	transaction := models.BankTransaction{
		BankTransactionID: id, PaymentIntentID: paymentIntentID, MerchantID: merchantID,
		Status: models.BankTransactionScheduled, CardNumberLastFour: lastFour,
		ExpiryMonth: card.ExpiryMonth, ExpiryYear: card.ExpiryYear, CreatedAt: r.now().UTC(),
		IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, OriginatingRequestID: originatingRequestID,
	}
	r.bankTransactions = append(r.bankTransactions, transaction)
	intent.Status = models.PaymentIntentProcessing
	intent.UpdatedAt = transaction.CreatedAt
	r.paymentIntents[index] = intent
	snapshot := cloneBankTransaction(transaction)
	change := transactionChange(transaction, models.BankTransactionCreated)
	change.BankTransaction = &snapshot
	if err := r.publishChanges(change, originalIntent, intent, originatingRequestID); err != nil {
		return nil, nil, nil, err
	}
	intentSnapshot = r.withBankTransactions(intent)
	transactionSnapshot := cloneBankTransaction(transaction)
	cardCopy := *card
	return &intentSnapshot, &transactionSnapshot, &cardCopy, nil
}

func (r *PaymentIntentsRepository) CompleteBankTransaction(merchantID, transactionID string, status models.BankTransactionStatus, authorizationCode *string) (models.BankTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completeBankTransactionLocked(merchantID, transactionID, status, authorizationCode)
}

func (r *PaymentIntentsRepository) completeBankTransactionLocked(merchantID, transactionID string, status models.BankTransactionStatus, authorizationCode *string) (models.BankTransaction, error) {
	index := r.findBankTransaction(merchantID, transactionID)
	if index < 0 {
		return models.BankTransaction{}, ErrBankTransactionNotFound
	}
	original := r.bankTransactions[index]
	if !status.RequiresVerification() && status != models.BankTransactionAuthorized && status != models.BankTransactionDeclined {
		return models.BankTransaction{}, errors.New("transaction completion requires a bank outcome or pending verification")
	}
	if original.Status != status && original.Status != models.BankTransactionScheduled && !original.Status.RequiresVerification() {
		return models.BankTransaction{}, errors.New("transaction cannot transition to the requested outcome")
	}
	if original.Status == status {
		if (original.AuthorizationCode == nil) != (authorizationCode == nil) ||
			(authorizationCode != nil && *original.AuthorizationCode != *authorizationCode) {
			return models.BankTransaction{}, errors.New("transaction already has a different authorization code")
		}
		return cloneBankTransaction(original), nil
	}
	intentIndex := r.findPaymentIntent(merchantID, original.PaymentIntentID)
	if intentIndex < 0 {
		return models.BankTransaction{}, models.ErrPaymentIntentNotFound
	}
	intentStatus, err := status.ToPaymentIntentStatus()
	if err != nil {
		return models.BankTransaction{}, err
	}
	completed := cloneBankTransaction(original)
	completed.Status = status
	completed.AuthorizationCode = clonePointer(authorizationCode)
	now := r.now().UTC()
	completed.CompletedAt = &now
	change := transactionChange(completed, models.BankTransactionUpdated)
	for _, field := range []struct {
		name          string
		before, after interface{}
	}{
		{"Status", original.Status, completed.Status},
		{"CompletedAt", original.CompletedAt, completed.CompletedAt},
		{"AuthorizationCode", original.AuthorizationCode, completed.AuthorizationCode},
	} {
		before, err := json.Marshal(field.before)
		if err != nil {
			return models.BankTransaction{}, fmt.Errorf("encode original transaction field: %w", err)
		}
		after, err := json.Marshal(field.after)
		if err != nil {
			return models.BankTransaction{}, fmt.Errorf("encode updated transaction field: %w", err)
		}
		if !bytes.Equal(before, after) {
			change.Changes = append(change.Changes, models.BankTransactionFieldChange{FieldName: field.name, OriginalValue: before, NewValue: after})
		}
	}
	originalIntent := r.paymentIntents[intentIndex]
	intent := originalIntent
	if intent.Status != intentStatus {
		intent.Status = intentStatus
		intent.UpdatedAt = now
	}
	r.bankTransactions[index] = completed
	r.paymentIntents[intentIndex] = intent
	if err := r.publishChanges(change, originalIntent, intent, completed.OriginatingRequestID); err != nil {
		return models.BankTransaction{}, err
	}
	return cloneBankTransaction(completed), nil
}

func (r *PaymentIntentsRepository) publishChanges(bankChange models.BankTransactionChangeEvent, before, after models.PaymentIntent, requestID string) error {
	var bankErr error
	if err := r.bankTransactionChange.Produce(bankChange); err != nil {
		bankErr = fmt.Errorf("bank transaction committed but change publication failed: %w", err)
	}
	// Each stream is independent: attempt both publications even if one fails.
	return errors.Join(bankErr, r.publishIntentChange(before, after, requestID))
}

func (r *PaymentIntentsRepository) publishIntentChange(before, after models.PaymentIntent, requestID string) error {
	if before.PaymentIntentID != "" && before.Status == after.Status {
		return nil
	}
	event := models.PaymentIntentChangeEvent{
		RequestID: requestID, MerchantID: after.MerchantID, PaymentIntentID: after.PaymentIntentID,
		EventType: models.PaymentIntentCreatedEvent,
	}
	if before.PaymentIntentID == "" {
		event.PaymentIntent = &models.PaymentIntentSnapshot{
			Status: after.Status, Currency: after.Currency, Amount: after.Amount,
			CreatedAt: after.CreatedAt, UpdatedAt: after.UpdatedAt,
		}
	} else {
		event.EventType = models.PaymentIntentUpdatedEvent
		for _, field := range []struct {
			name          string
			before, after any
		}{
			{"Status", before.Status, after.Status},
			{"UpdatedAt", before.UpdatedAt, after.UpdatedAt},
		} {
			original, err := json.Marshal(field.before)
			if err != nil {
				return fmt.Errorf("encode original payment intent change: %w", err)
			}
			current, err := json.Marshal(field.after)
			if err != nil {
				return fmt.Errorf("encode new payment intent change: %w", err)
			}
			if !bytes.Equal(original, current) {
				event.Changes = append(event.Changes, models.PaymentIntentFieldChange{
					FieldName: field.name, OriginalValue: original, NewValue: current,
				})
			}
		}
	}
	if err := r.paymentIntentChange.Produce(event); err != nil {
		return fmt.Errorf("payment intent committed but change publication failed: %w", err)
	}
	return nil
}
func (r *PaymentIntentsRepository) GetTimedOutBankTransactions(cutoff time.Time, batchSize int) ([]models.BankTransaction, error) {
	if batchSize <= 0 {
		return nil, errors.New("batch size must be positive")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	transactions := make([]models.BankTransaction, 0)
	for _, transaction := range r.bankTransactions {
		if transaction.Status == models.BankTransactionScheduled && transaction.CompletedAt == nil && !transaction.CreatedAt.After(cutoff) {
			transactions = append(transactions, cloneBankTransaction(transaction))
		}
	}
	sort.Slice(transactions, func(i, j int) bool {
		if transactions[i].CreatedAt.Equal(transactions[j].CreatedAt) {
			return transactions[i].BankTransactionID < transactions[j].BankTransactionID
		}
		return transactions[i].CreatedAt.Before(transactions[j].CreatedAt)
	})
	if len(transactions) > batchSize {
		transactions = transactions[:batchSize]
	}
	return transactions, nil
}

func (r *PaymentIntentsRepository) TryMarkTimedOutBankTransaction(merchantID, transactionID string, cutoff time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.findBankTransaction(merchantID, transactionID)
	if index < 0 {
		return false, ErrBankTransactionNotFound
	}
	transaction := r.bankTransactions[index]
	if transaction.Status != models.BankTransactionScheduled || transaction.CompletedAt != nil || transaction.CreatedAt.After(cutoff) {
		return false, nil
	}
	intentIndex := r.findPaymentIntent(merchantID, transaction.PaymentIntentID)
	if intentIndex < 0 {
		return false, models.ErrPaymentIntentNotFound
	}
	intentStatus := r.paymentIntents[intentIndex].Status
	if intentStatus != models.PaymentIntentProcessing && intentStatus != models.PaymentIntentPendingVerification {
		return false, nil
	}
	_, err := r.completeBankTransactionLocked(merchantID, transactionID, models.BankTransactionStatusUpdateTimeout, nil)
	return err == nil, err
}

func (r *PaymentIntentsRepository) GetPaymentIntent(merchantID, paymentIntentID string) (*models.PaymentIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.findPaymentIntent(merchantID, paymentIntentID)
	if index < 0 {
		return nil, nil
	}
	intent := r.withBankTransactions(r.paymentIntents[index])
	return &intent, nil
}

func (r *PaymentIntentsRepository) GetBankTransaction(merchantID, transactionID string) (*models.BankTransaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.findBankTransaction(merchantID, transactionID)
	if index < 0 {
		return nil, nil
	}
	transaction := cloneBankTransaction(r.bankTransactions[index])
	return &transaction, nil
}

func (r *PaymentIntentsRepository) fingerprint(value interface{}) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode payment request fingerprint: %w", err)
	}
	mac := hmac.New(sha256.New, r.fingerprintKey[:])
	_, _ = mac.Write(data)
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil))), nil
}

func (r *PaymentIntentsRepository) findPaymentIntent(merchantID, id string) int {
	for i, intent := range r.paymentIntents {
		if intent.MerchantID == merchantID && intent.PaymentIntentID == id {
			return i
		}
	}
	return -1
}

func (r *PaymentIntentsRepository) findBankTransaction(merchantID, id string) int {
	for i, transaction := range r.bankTransactions {
		if transaction.MerchantID == merchantID && transaction.BankTransactionID == id {
			return i
		}
	}
	return -1
}

func (r *PaymentIntentsRepository) withBankTransactions(intent models.PaymentIntent) models.PaymentIntent {
	intent.BankTransactions = make([]models.BankTransaction, 0)
	for _, transaction := range r.bankTransactions {
		if transaction.MerchantID == intent.MerchantID && transaction.PaymentIntentID == intent.PaymentIntentID {
			intent.BankTransactions = append(intent.BankTransactions, cloneBankTransaction(transaction))
		}
	}
	sort.SliceStable(intent.BankTransactions, func(i, j int) bool {
		return intent.BankTransactions[i].CreatedAt.Before(intent.BankTransactions[j].CreatedAt)
	})
	return intent
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBankTransaction(transaction models.BankTransaction) models.BankTransaction {
	transaction.CompletedAt = clonePointer(transaction.CompletedAt)
	transaction.AuthorizationCode = clonePointer(transaction.AuthorizationCode)
	return transaction
}

func transactionChange(transaction models.BankTransaction, eventType models.BankTransactionEventType) models.BankTransactionChangeEvent {
	return models.BankTransactionChangeEvent{
		RequestID: transaction.OriginatingRequestID, EventType: eventType, MerchantID: transaction.MerchantID,
		PaymentIntentID: transaction.PaymentIntentID, BankTransactionID: transaction.BankTransactionID,
		Changes: make([]models.BankTransactionFieldChange, 0),
	}
}
