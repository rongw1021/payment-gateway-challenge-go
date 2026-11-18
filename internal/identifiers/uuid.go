package identifiers

import (
	"fmt"

	"github.com/google/uuid"
)

func NewUUID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	return id.String(), nil
}

func IsCanonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}
