package a

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func productionBoundary(t *testing.T) {
	assert.Eventually(t, func() bool { return true }, 999*time.Millisecond, time.Millisecond)
}
