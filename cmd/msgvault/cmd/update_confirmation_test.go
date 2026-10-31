package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/update"
	"strings"
	"testing"
)

func TestUpdateClosedConfirmationRequiresYes(t *testing.T) {
	oldCheck, oldDistribution := checkForUpdate, Distribution
	t.Cleanup(func() { checkForUpdate, Distribution = oldCheck, oldDistribution })
	Distribution = ""
	// Only the external release lookup is replaced; the update decision and prompt run normally.
	checkForUpdate = func(string, bool) (*update.UpdateInfo, error) {
		return &update.UpdateInfo{CurrentVersion: "v0.1.0", LatestVersion: "v0.2.0"}, nil
	}
	command := newUpdateCmd()
	command.SetIn(strings.NewReader(""))
	err := runUpdate(command, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "--yes")
}
