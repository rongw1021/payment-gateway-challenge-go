package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

const (
	CardTokensCreate      = "card-tokens:create"
	PaymentIntentsCreate  = "payment-intents:create"
	PaymentIntentsConfirm = "payment-intents:confirm"
	PaymentIntentsRead    = "payment-intents:read"
)

var ErrUnauthorized = errors.New("unauthorized")

type APIKeyCredential struct {
	MerchantID string   `json:"merchant_id" yaml:"merchant_id"`
	KeyHash    string   `json:"key_hash" yaml:"key_hash"`
	Scopes     []string `json:"scopes" yaml:"scopes"`
}

type credential struct {
	merchant Merchant
	hash     [sha256.Size]byte
}

type Merchant struct {
	ID     string
	scopes []string
}

func (m Merchant) Allows(scope string) bool {
	for _, granted := range m.scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

type Authenticator struct {
	credentials []credential
}

func NewAuthenticator(credentials []APIKeyCredential) (*Authenticator, error) {
	if len(credentials) == 0 {
		return nil, errors.New("at least one API key must be configured")
	}
	a := &Authenticator{}
	hashes := make(map[[sha256.Size]byte]bool)
	for i, entry := range credentials {
		if strings.TrimSpace(entry.MerchantID) == "" {
			return nil, fmt.Errorf("API key %d: merchant_id is required", i)
		}
		hash, err := hex.DecodeString(entry.KeyHash)
		if err != nil || len(hash) != sha256.Size {
			return nil, fmt.Errorf("API key %d: key_hash must contain 64 hexadecimal characters", i)
		}
		var keyHash [sha256.Size]byte
		copy(keyHash[:], hash)
		if hashes[keyHash] {
			return nil, fmt.Errorf("API key %d: key_hash must be unique", i)
		}
		hashes[keyHash] = true
		if entry.Scopes == nil {
			return nil, fmt.Errorf("API key %d: scopes must be an array", i)
		}
		for _, scope := range entry.Scopes {
			switch scope {
			case CardTokensCreate, PaymentIntentsRead, PaymentIntentsCreate, PaymentIntentsConfirm:
			default:
				return nil, fmt.Errorf("API key %d: unsupported scope", i)
			}
		}
		a.credentials = append(a.credentials, credential{
			merchant: Merchant{ID: entry.MerchantID, scopes: append([]string(nil), entry.Scopes...)},
			hash:     keyHash,
		})
	}
	return a, nil
}

func (a *Authenticator) Authenticate(headers http.Header) (Merchant, error) {
	values := headers.Values("Authorization")
	if len(values) != 1 {
		return Merchant{}, ErrUnauthorized
	}
	scheme, apiKey, found := strings.Cut(strings.TrimSpace(values[0]), " ")
	apiKey = strings.TrimSpace(apiKey)
	if !found || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(apiKey, "sk_") ||
		len(apiKey) <= 3 || strings.ContainsAny(apiKey[3:], "_,") || strings.ContainsFunc(apiKey, unicode.IsSpace) {
		return Merchant{}, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(apiKey[3:]))
	for _, entry := range a.credentials {
		if subtle.ConstantTimeCompare(hash[:], entry.hash[:]) == 1 {
			return entry.merchant, nil
		}
	}
	return Merchant{}, ErrUnauthorized
}
