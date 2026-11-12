package beeperidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFallbackScopedEncoding(t *testing.T) {
	assert.Equal(t, "beeper:9:account:a:8:user:雪", Fallback(" account:a ", " user:雪 "))
}
