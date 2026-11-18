package repository

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardsRepositoryRejectsDuplicateWithoutReplacingCard(t *testing.T) {
	now := time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC)
	repo := NewCardsRepository(func() time.Time { return now })
	hash := sha256.Sum256([]byte("token"))
	card := models.CreateCardTokenRequest{CardNumber: "0222405343240007", ExpiryMonth: 4, ExpiryYear: 2035, Cvv: "012"}
	require.NoError(t, repo.Add("merchant", hash, card, now.Add(time.Minute)))
	assert.ErrorIs(t, repo.Add("merchant", hash, models.CreateCardTokenRequest{}, now), ErrDuplicateCardToken)
	require.NoError(t, repo.Add("other", hash, card, now.Add(time.Minute)))

	for _, merchant := range []string{"merchant", "other"} {
		consumed, err := repo.TryConsume(merchant, hash)
		require.NoError(t, err)
		require.NotNil(t, consumed)
		assert.Equal(t, card, *consumed)
		consumed, err = repo.TryConsume(merchant, hash)
		require.NoError(t, err)
		assert.Nil(t, consumed)
	}
}

func TestCardsRepositoryConsumePurgesExpiredEntries(t *testing.T) {
	for _, requested := range []string{"expired", "boundary", "live", "unknown"} {
		t.Run(requested, func(t *testing.T) {
			now := time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC)
			repo := NewCardsRepository(func() time.Time { return now })
			card := models.CreateCardTokenRequest{CardNumber: "0222405343240007", Cvv: "012"}
			for token, expiresAt := range map[string]time.Time{
				"expired": now.Add(-time.Nanosecond), "boundary": now, "live": now.Add(time.Nanosecond),
			} {
				require.NoError(t, repo.Add("merchant", sha256.Sum256([]byte(token)), card, expiresAt))
			}
			otherLive := sha256.Sum256([]byte("other-live"))
			require.NoError(t, repo.Add("other", sha256.Sum256([]byte("other-expired")), card, now))
			require.NoError(t, repo.Add("other", otherLive, card, now.Add(time.Minute)))

			consumed, err := repo.TryConsume("merchant", sha256.Sum256([]byte(requested)))
			require.NoError(t, err)
			if requested == "live" {
				require.NotNil(t, consumed)
				assert.Equal(t, card, *consumed)
				assert.Len(t, repo.tokens, 1)
			} else {
				assert.Nil(t, consumed)
				assert.Len(t, repo.tokens, 2)
				assert.Contains(t, repo.tokens, cardTokenKey{"merchant", sha256.Sum256([]byte("live"))})
			}
			assert.Contains(t, repo.tokens, cardTokenKey{"other", otherLive})
		})
	}
}
