package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The daemon-only CLI model means a command missing from the run allowlist
// simply does not work end-to-end, with no compile-time signal — so the
// Telegram import's presence is asserted explicitly.
func TestCLIRunCommandAllowedTelegramCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"import-telegram"},
		{"import-telegram", "--me=482111", "/tmp/TelegramExport"},
		{"sync-telegram", "--full"},
	} {
		t.Run(args[0], func(t *testing.T) {
			assert.True(t, cliRunCommandAllowed(args), "%v must be runnable via the daemon CLI", args)
		})
	}
	assert.False(t, cliRunCommandAllowed([]string{"telegram-not-a-command"}))
}
