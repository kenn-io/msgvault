package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPocketCommands(t *testing.T) {
	assert.Equal(t, "add-pocket [identifier]", newAddPocketCmd().Use)
	assert.Equal(t, "sync-pocket [identifier]", newSyncPocketCmd().Use)
}
