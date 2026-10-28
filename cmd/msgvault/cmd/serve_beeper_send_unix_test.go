//go:build darwin || linux

package cmd

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"golang.org/x/sys/unix"
)

func TestBeeperSendRefusesFIFO(t *testing.T) {
	f := newBeeperDraftFixture(t)
	f.adapter.beeperSendPolicy = []config.GmailDraftSource{{SourceID: f.source.ID, Enabled: true}}
	_, err := f.store.EnsureConversationWithType(f.source.ID, beeperDraftTestChat, "direct", "Example chat")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "attachment.fifo")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	req := api.CLIRunRequest{Args: []string{"send-message", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", beeperDraftTestChat, "--file", path}}
	result := make(chan error, 1)
	go func() {
		result <- f.adapter.runCLISendMessage(t.Context(), req, nil)
	}()
	select {
	case err := <-result:
		assertBeeperDraftCode(t, err, "invalid_args")
	case <-time.After(5 * time.Second):
		// Release a blocked reader so a regression does not leave a goroutine behind.
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
		require.NoError(t, err)
		defer func() { assert.NoError(t, unix.Close(fd)) }()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
		}
		require.FailNow(t, "FIFO attachment was not refused promptly")
	}
	requests, _ := f.beeper.counts()
	assert.Zero(t, requests)
}
