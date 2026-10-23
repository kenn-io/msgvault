package novelty

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestGatesEligible(t *testing.T) {
	for _, tc := range []struct {
		identity, runes int
		cooled, want    bool
	}{{799, 40, false, false}, {800, 40, false, true}, {1000, 40, false, true}, {800, 39, false, false}, {800, 40, true, false}} {
		assert.Equal(t, tc.want, DefaultGates().Eligible(tc.identity, tc.runes, tc.cooled))
	}
}
