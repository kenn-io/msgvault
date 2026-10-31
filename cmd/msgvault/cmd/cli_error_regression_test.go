package cmd

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestProxyProgressDoesNotSuppressGuidance(t *testing.T) {
	for _, progress := range []string{`info: {"error":{"message":"example"}}` + "\n", "Error: a quoted example\nprogress continues\n"} {
		err := proxiedChildError{stderr: &childStderr{tail: []byte(progress)}}
		assert.False(t, err.alreadyReported(), progress)
	}
}

func TestImportGroupDoesNotEmitLegacyNotice(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"import", "--type", "whatsapp"})
	require.Error(t, executeRootContext(t.Context(), rootCmd))
	assert.NotContains(t, out.String(), "deprecated")
}
