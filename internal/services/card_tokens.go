package services

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/identifiers"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/requestcontext"
)

type CardsRepository interface {
	Add(string, [sha256.Size]byte, models.CreateCardTokenRequest, time.Time) error
	TryConsume(string, [sha256.Size]byte) (*models.CreateCardTokenRequest, error)
}

type CardTokenService struct {
	repository CardsRepository
	now        func() time.Time
	logger     *slog.Logger
}

func NewCardTokenService(repository CardsRepository, now func() time.Time, logger *slog.Logger) *CardTokenService {
	return &CardTokenService{repository: repository, now: now, logger: logger}
}

type ValidationError struct {
	Errors map[string][]string
}

func (*ValidationError) Error() string { return "one or more validation errors occurred" }

func (s *CardTokenService) Issue(ctx context.Context, merchantID string, card models.CreateCardTokenRequest) (*models.CardTokenResponse, error) {
	if strings.TrimSpace(merchantID) == "" {
		return nil, errors.New("merchant ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if cardHasExpired(card, now) {
		return nil, &ValidationError{Errors: map[string][]string{
			"expiry_month": {"The card has expired."},
			"expiry_year":  {"The card has expired."},
		}}
	}
	token, err := identifiers.NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate card token: %w", err)
	}
	expiresAt := now.Add(15 * time.Minute)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.repository.Add(merchantID, sha256.Sum256([]byte(token)), card, expiresAt); err != nil {
		return nil, fmt.Errorf("store card token: %w", err)
	}
	return &models.CardTokenResponse{Token: token, ExpiresAt: expiresAt}, nil
}

func (s *CardTokenService) TryConsume(ctx context.Context, merchantID, token string) (*models.CreateCardTokenRequest, error) {
	if strings.TrimSpace(merchantID) == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("merchant ID and token are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	card, err := s.repository.TryConsume(merchantID, sha256.Sum256([]byte(token)))
	if err != nil {
		return nil, fmt.Errorf("consume card token: %w", err)
	}
	if card == nil {
		ctx = context.WithValue(ctx, requestcontext.MerchantIDKey{}, merchantID)
		s.logger.InfoContext(ctx, "card token consumption rejected: token is unavailable or expired")
	}
	return card, nil
}

func cardHasExpired(card models.CreateCardTokenRequest, now time.Time) bool {
	return card.ExpiryYear < now.Year() || (card.ExpiryYear == now.Year() && card.ExpiryMonth < int(now.Month()))
}
