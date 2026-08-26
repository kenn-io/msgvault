package peoplesweep

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexRPCStderrEventPublicationNeverBlocks(t *testing.T) {
	client := CodexRPCClient{stderrEvent: make(chan error, 1)}
	client.stderrEvent <- errCodexStderrLimit
	done := make(chan struct{})
	go func() {
		client.publishStderrEvent(errCodexStderrRead)
		close(done)
	}()

	select {
	case <-done:
	case <-t.Context().Done():
		require.FailNow(t, "publishing a secondary stderr event blocked")
	}
	require.ErrorIs(t, <-client.stderrEvent, errCodexStderrLimit)
	require.NotErrorIs(t, client.checkStderr(), errCodexStderrRead)
}
