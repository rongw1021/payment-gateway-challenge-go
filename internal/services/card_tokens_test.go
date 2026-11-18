package services

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validCard() models.CreateCardTokenRequest {
	return models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 2035, Cvv: "012"}
}

func cardService(t *testing.T) (*CardTokenService, *repository.CardsRepository, *atomic.Int64) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	repo := repository.NewCardsRepository(now)
	return NewCardTokenService(repo, now), repo, clock
}

func TestCardTokenIssueStoresOnlyHashWithExactLifetime(t *testing.T) {
	service, repo, clock := cardService(t)
	card := validCard()
	token, err := service.Issue(context.Background(), "merchant", card)
	require.NoError(t, err)
	require.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", token.Token)
	assert.Equal(t, time.Unix(0, clock.Load()).UTC().Add(15*time.Minute), token.ExpiresAt)
	consumed, err := repo.TryConsume("merchant", sha256.Sum256([]byte(token.Token)))
	require.NoError(t, err)
	require.NotNil(t, consumed)
	assert.Equal(t, card, *consumed)
	another, err := service.Issue(context.Background(), "merchant", card)
	require.NoError(t, err)
	assert.NotEqual(t, token.Token, another.Token)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		assert.Equal(t, "CreateCardTokenRequest", fmt.Sprintf(format, card))
		assert.Equal(t, "CardTokenResponse", fmt.Sprintf(format, token))
	}
}

func TestCardTokenExactExpiryBoundary(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			service, _, clock := cardService(t)
			token, err := service.Issue(context.Background(), "merchant", validCard())
			require.NoError(t, err)
			clock.Store(token.ExpiresAt.Add(offset).UnixNano())
			card, err := service.TryConsume(context.Background(), "merchant", token.Token)
			require.NoError(t, err)
			if offset < 0 {
				require.NotNil(t, card)
				assert.Equal(t, validCard(), *card)
			} else {
				assert.Nil(t, card)
			}
			card, err = service.TryConsume(context.Background(), "merchant", token.Token)
			require.NoError(t, err)
			assert.Nil(t, card)
		})
	}
}

func TestCardTokenWrongMerchantAndUnknownTokenDoNotConsumeOrExtend(t *testing.T) {
	service, _, clock := cardService(t)
	token, err := service.Issue(context.Background(), "merchant", validCard())
	require.NoError(t, err)
	clock.Add(int64(14 * time.Minute))
	for _, attempt := range []struct{ merchant, token string }{
		{"other", token.Token}, {"merchant", "unknown"},
	} {
		card, err := service.TryConsume(context.Background(), attempt.merchant, attempt.token)
		require.NoError(t, err)
		assert.Nil(t, card)
	}
	card, err := service.TryConsume(context.Background(), "merchant", token.Token)
	require.NoError(t, err)
	require.NotNil(t, card)

	token, err = service.Issue(context.Background(), "merchant", validCard())
	require.NoError(t, err)
	clock.Add(int64(14 * time.Minute))
	_, err = service.TryConsume(context.Background(), "other", token.Token)
	require.NoError(t, err)
	clock.Add(int64(time.Minute))
	card, err = service.TryConsume(context.Background(), "merchant", token.Token)
	require.NoError(t, err)
	assert.Nil(t, card)
}

func TestCardTokenConcurrentConsumersAcrossServices(t *testing.T) {
	service, repo, _ := cardService(t)
	other := NewCardTokenService(repo, service.now)
	token, err := service.Issue(context.Background(), "merchant", validCard())
	require.NoError(t, err)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		consumer := service
		if i%2 == 0 {
			consumer = other
		}
		wg.Add(1)
		go func(s *CardTokenService) {
			defer wg.Done()
			card, err := s.TryConsume(context.Background(), "merchant", token.Token)
			assert.NoError(t, err)
			if card != nil {
				assert.Equal(t, validCard(), *card)
				successes.Add(1)
			}
		}(consumer)
	}
	wg.Wait()
	assert.EqualValues(t, 1, successes.Load())
}

func TestCardTokenCancellationDoesNotConsume(t *testing.T) {
	service, _, _ := cardService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	token, err := service.Issue(ctx, "merchant", validCard())
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, token)
	token, err = service.Issue(context.Background(), "merchant", validCard())
	require.NoError(t, err)
	card, err := service.TryConsume(ctx, "merchant", token.Token)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, card)
	card, err = service.TryConsume(context.Background(), "merchant", token.Token)
	require.NoError(t, err)
	require.NotNil(t, card)
}

func TestCardTokenExpiryValidation(t *testing.T) {
	service, _, _ := cardService(t)
	for _, test := range []struct {
		name   string
		field  string
		change func(*models.CreateCardTokenRequest)
	}{
		{"past year", "expiry_year", func(c *models.CreateCardTokenRequest) { c.ExpiryYear = 2029 }},
		{"past month", "expiry_month", func(c *models.CreateCardTokenRequest) { c.ExpiryYear, c.ExpiryMonth = 2030, 3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := validCard()
			test.change(&card)
			token, err := service.Issue(context.Background(), "merchant", card)
			var validation *ValidationError
			require.ErrorAs(t, err, &validation)
			assert.Contains(t, validation.Errors, test.field)
			assert.Nil(t, token)
		})
	}
	card := validCard()
	card.ExpiryMonth, card.ExpiryYear = 4, 2030
	_, err := service.Issue(context.Background(), "merchant", card)
	require.NoError(t, err)
}

type failingCardsRepository struct {
	err error
}

func (r failingCardsRepository) Add(string, [sha256.Size]byte, models.CreateCardTokenRequest, time.Time) error {
	return r.err
}

func (r failingCardsRepository) TryConsume(string, [sha256.Size]byte) (*models.CreateCardTokenRequest, error) {
	return nil, r.err
}

func TestCardTokenPropagatesRepositoryFailures(t *testing.T) {
	service, _, _ := cardService(t)
	failure := errors.New("storage unavailable")
	service.repository = failingCardsRepository{err: failure}
	_, err := service.Issue(context.Background(), "merchant", validCard())
	assert.ErrorIs(t, err, failure)
	_, err = service.TryConsume(context.Background(), "merchant", "token")
	assert.ErrorIs(t, err, failure)
}

type successfulCardsRepository struct{ calls int }

func (r *successfulCardsRepository) Add(string, [sha256.Size]byte, models.CreateCardTokenRequest, time.Time) error {
	r.calls++
	return nil
}

func (r *successfulCardsRepository) TryConsume(string, [sha256.Size]byte) (*models.CreateCardTokenRequest, error) {
	r.calls++
	card := validCard()
	return &card, nil
}

func TestCardTokenRejectsMissingIdentityAndTokenBeforeStorage(t *testing.T) {
	service, _, _ := cardService(t)
	repo := &successfulCardsRepository{}
	service.repository = repo
	_, err := service.Issue(context.Background(), " ", validCard())
	require.ErrorContains(t, err, "merchant ID is required")
	for _, attempt := range []struct{ merchant, token string }{{"", "token"}, {"merchant", " "}} {
		_, err := service.TryConsume(context.Background(), attempt.merchant, attempt.token)
		require.ErrorContains(t, err, "merchant ID and token are required")
	}
	assert.Zero(t, repo.calls)
}
