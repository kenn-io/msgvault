//go:build !sqlite_vec && !pgvector

package cmd

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestDocumentVectorStubKeepsStatusAvailableAndBuildActionable(t *testing.T) {
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg = config.NewDefaultConfig()
	status := newDocumentsCmd(documentsCommandDeps{})
	var output bytes.Buffer
	status.SetOut(&output)
	status.SetArgs([]string{documentVectorsSubcommand, statusValue, "--json"})
	require.NoError(t, status.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"enabled":false}`, output.String())

	_, err := runConfiguredDocumentVectorGeneration(t.Context(), nil, 1, 1)
	require.ErrorContains(t, err, "rebuild with sqlite_vec or pgvector support")
}
