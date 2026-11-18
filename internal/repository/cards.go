package repository

import (
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/models"
)

var ErrDuplicateCardToken = errors.New("card token already exists")

type cardTokenKey struct {
	merchantID string
	hash       [sha256.Size]byte
}

type cardTokenEntry struct {
	card      models.CreateCardTokenRequest
	expiresAt time.Time
}

type CardsRepository struct {
	mu     sync.Mutex
	tokens map[cardTokenKey]cardTokenEntry
	now    func() time.Time
}

func NewCardsRepository(now func() time.Time) *CardsRepository {
	return &CardsRepository{
		tokens: make(map[cardTokenKey]cardTokenEntry),
		now:    now,
	}
}

func (r *CardsRepository) Add(merchantID string, hash [sha256.Size]byte, card models.CreateCardTokenRequest, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := cardTokenKey{merchantID, hash}
	if _, exists := r.tokens[key]; exists {
		return ErrDuplicateCardToken
	}
	r.tokens[key] = cardTokenEntry{card, expiresAt}
	return nil
}

func (r *CardsRepository) TryConsume(merchantID string, hash [sha256.Size]byte) (*models.CreateCardTokenRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for key, entry := range r.tokens {
		if !now.Before(entry.expiresAt) {
			delete(r.tokens, key)
		}
	}
	key := cardTokenKey{merchantID, hash}
	entry, exists := r.tokens[key]
	delete(r.tokens, key)
	if !exists {
		return nil, nil
	}
	return &entry.card, nil
}
