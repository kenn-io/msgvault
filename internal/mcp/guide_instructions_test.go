package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstructionsPointer(t *testing.T) {
	assertions := assert.New(t)
	for _, delegated := range []bool{false, true} {
		session := task5ConnectClient(t, ServeOptions{DelegatedOnly: delegated}, false)
		initialized := session.InitializeResult()
		require.NotNil(t, initialized)
		assertions.True(strings.HasPrefix(initialized.Instructions, archiveSafetyInstructions))
		assertions.Contains(initialized.Instructions, "https://msgvault.io/docs/guides/agent-quickstart.md (msgvault quickstart --mcp)")
		if delegated {
			assertions.Contains(initialized.Instructions, "the user controls sending")
		}
	}
}
