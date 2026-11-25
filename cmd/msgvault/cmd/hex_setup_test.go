package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHexSetupBeforeFirstMeeting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hex", "meetings")
	require.NoError(t, probeHexMeetings(t.Context(), path))
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
