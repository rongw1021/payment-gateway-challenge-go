package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/integrations/banksimulator"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/requestcontext"
)

type PaymentIntentsRepository interface {
	CreatePaymentIntent(string, string, string, models.CreatePaymentIntentRequest) (models.PaymentIntent, bool, error)
	CreateBankTransaction(string, string, string, string, models.ConfirmPaymentIntentRequest, func() (*models.CreateCardTokenRequest, error)) (*models.PaymentIntent, *models.BankTransaction, *models.CreateCardTokenRequest, error)
	CompleteBankTransaction(string, string, models.BankTransactionStatus, *string) (models.BankTransaction, error)
	GetPaymentIntent(string, string) (*models.PaymentIntent, error)
}

type CardTokenConsumer interface {
	TryConsume(context.Context, string, string) (*models.CreateCardTokenRequest, error)
}

type PaymentIntentsService struct {
	repository PaymentIntentsRepository
	bank       banksimulator.Client
	cardTokens CardTokenConsumer
	now        func() time.Time
	logger     *slog.Logger
}

func NewPaymentIntentsService(repo PaymentIntentsRepository, bank banksimulator.Client, cardTokens CardTokenConsumer, now func() time.Time, logger *slog.Logger) (*PaymentIntentsService, error) {
	if repo == nil || bank == nil || cardTokens == nil || now == nil || logger == nil {
		return nil, errors.New("payment intents repository, bank client, card token service, clock and logger are required")
	}
	return &PaymentIntentsService{repository: repo, bank: bank, cardTokens: cardTokens, now: now, logger: logger}, nil
}

func (s *PaymentIntentsService) CreatePaymentIntent(ctx context.Context, merchantID, idempotencyKey string, request models.CreatePaymentIntentRequest) (*models.PaymentIntentResponse, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	intent, hasConflict, err := s.repository.CreatePaymentIntent(merchantID, idempotencyKey, middleware.GetRequestID(ctx), request)
	if err != nil {
		return nil, false, fmt.Errorf("create payment intent: %w", err)
	}
	return paymentIntentResponse(intent), hasConflict, nil
}

func (s *PaymentIntentsService) ConfirmPaymentIntent(
	ctx context.Context, merchantID, paymentIntentID, idempotencyKey string,
	request models.ConfirmPaymentIntentRequest,
) (*models.PaymentIntentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	originatingRequestID := middleware.GetRequestID(ctx)
	ctx = context.WithValue(ctx, requestcontext.MerchantIDKey{}, merchantID)
	ctx = context.WithValue(ctx, requestcontext.PaymentIntentIDKey{}, paymentIntentID)
	intent, transaction, card, err := s.repository.CreateBankTransaction(
		merchantID, paymentIntentID, idempotencyKey, originatingRequestID, request,
		func() (*models.CreateCardTokenRequest, error) {
			card, err := s.cardTokens.TryConsume(ctx, merchantID, request.CardToken)
			if err != nil {
				return nil, err
			}
			if card != nil && cardHasExpired(*card, s.now().UTC()) {
				s.logger.InfoContext(ctx, "payment confirmation rejected: tokenized card has expired")
				return nil, nil
			}
			return card, nil
		})
	if err != nil {
		return nil, fmt.Errorf("create bank transaction: %w", err)
	}
	if card == nil {
		return paymentIntentResponse(*intent), nil
	}
	ctx = context.WithValue(ctx, requestcontext.BankTransactionIDKey{}, transaction.BankTransactionID)

	decision, bankErr := s.bank.ProcessBankTransaction(ctx, banksimulator.BankTransactionRequest{
		CardNumber: card.CardNumber, ExpiryDate: fmt.Sprintf("%02d/%04d", card.ExpiryMonth, card.ExpiryYear),
		Currency: intent.Currency, Amount: intent.Amount, Cvv: card.Cvv,
	})
	if bankErr == nil && decision.Authorized &&
		(decision.AuthorizationCode == nil || strings.TrimSpace(*decision.AuthorizationCode) == "") {
		bankErr = &banksimulator.Failure{Kind: banksimulator.InvalidResponseFailure}
	}
	if bankErr != nil {
		status, recognized := classifyBankFailure(ctx, bankErr)
		if !recognized {
			return nil, fmt.Errorf("process bank transaction: %w", bankErr)
		}
		s.logger.InfoContext(ctx, "payment intent requires verification after bank failure", "bank_transaction_status", string(status))
		_, err := s.repository.CompleteBankTransaction(merchantID, transaction.BankTransactionID, status, nil)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("record bank failure: %w", err), bankErr)
		}
		if status == models.BankTransactionRequestCanceled && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return s.GetPaymentIntent(ctx, merchantID, paymentIntentID)
	}
	status := models.BankTransactionDeclined
	if decision.Authorized {
		status = models.BankTransactionAuthorized
	}
	_, err = s.repository.CompleteBankTransaction(merchantID, transaction.BankTransactionID, status, decision.AuthorizationCode)
	if err != nil {
		return nil, fmt.Errorf("complete bank transaction: %w", err)
	}
	return s.GetPaymentIntent(ctx, merchantID, paymentIntentID)
}

func (s *PaymentIntentsService) GetPaymentIntent(ctx context.Context, merchantID, paymentIntentID string) (*models.PaymentIntentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	intent, err := s.repository.GetPaymentIntent(merchantID, paymentIntentID)
	if err != nil {
		return nil, fmt.Errorf("get payment intent: %w", err)
	}
	if intent == nil {
		return nil, nil
	}
	return paymentIntentResponse(*intent), nil
}

func classifyBankFailure(ctx context.Context, err error) (models.BankTransactionStatus, bool) {
	timeoutStatus := models.BankTransactionRequestTimeout
	if ctx.Err() != nil {
		timeoutStatus = models.BankTransactionRequestCanceled
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return timeoutStatus, true
	}
	var failure *banksimulator.Failure
	if errors.As(err, &failure) {
		switch failure.Kind {
		case banksimulator.HTTPFailure:
			if failure.StatusCode == http.StatusServiceUnavailable {
				return models.BankTransactionServiceUnavailable, true
			}
			return models.BankTransactionInvalidResponse, true
		case banksimulator.NetworkFailure:
			return models.BankTransactionNetworkError, true
		case banksimulator.InvalidResponseFailure:
			return models.BankTransactionInvalidResponse, true
		case banksimulator.TimeoutFailure:
			return timeoutStatus, true
		}
	}
	var syntax *json.SyntaxError
	var jsonType *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &jsonType) {
		return models.BankTransactionInvalidResponse, true
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return timeoutStatus, true
		}
		return models.BankTransactionNetworkError, true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return models.BankTransactionNetworkError, true
	}
	return "", false
}

func paymentIntentResponse(intent models.PaymentIntent) *models.PaymentIntentResponse {
	transactions := make([]models.BankTransactionResponse, 0, len(intent.BankTransactions))
	for _, transaction := range intent.BankTransactions {
		transactions = append(transactions, models.BankTransactionResponse{
			BankTransactionID: transaction.BankTransactionID, Status: transaction.Status,
			CardNumberLastFour: transaction.CardNumberLastFour, ExpiryMonth: transaction.ExpiryMonth, ExpiryYear: transaction.ExpiryYear,
			CreatedAt: transaction.CreatedAt, UpdatedAt: transaction.UpdatedAt,
		})
	}
	return &models.PaymentIntentResponse{
		PaymentIntentID: intent.PaymentIntentID, Status: intent.Status, Currency: intent.Currency, Amount: intent.Amount,
		BankTransactions: transactions,
		CreatedAt:        intent.CreatedAt, UpdatedAt: intent.UpdatedAt,
	}
}
