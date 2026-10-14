package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestCalDAVDaemonCLICommand(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"sync-caldav", "personal", "--full"}))
}
