package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLineSenderAttributionUsesPlatformIdentity(t *testing.T) {
	assert.True(t, personSweepSourceAuthenticatesSender("line"))
	assert.True(t, personSweepSourceAuthenticatesSender(" LINE "))
	assert.False(t, personSweepSourceAuthenticatesSender("line-import"))
}
