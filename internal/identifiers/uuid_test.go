package identifiers

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCanonicalUUID(t *testing.T) {
	const valid = "01234567-89ab-4cde-8123-456789abcdef"
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{"canonical", valid, true},
		{"nil UUID", "00000000-0000-0000-0000-000000000000", true},
		{"empty", "", false},
		{"uppercase", strings.ToUpper(valid), false},
		{"no hyphens", strings.ReplaceAll(valid, "-", ""), false},
		{"braces", "{" + valid + "}", false},
		{"URN", "urn:uuid:" + valid, false},
		{"short", valid[:35], false},
		{"long", valid + "0", false},
		{"non-hex", valid[:35] + "g", false},
		{"wrong separators", strings.ReplaceAll(valid, "-", "_"), false},
		{"surrounding spaces", " " + valid + " ", false},
		{"newline", valid + "\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.valid, IsCanonicalUUID(test.value))
		})
	}
}
