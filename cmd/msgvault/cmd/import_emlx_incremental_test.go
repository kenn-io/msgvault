package cmd

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"testing"
)

func TestImportEmlxIncrementalFlagAndSummary(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	// Parse the real Cobra flag set and restore its global bound value.
	old := importEmlxFullReconcile
	flag := importEmlxCmd.Flags().Lookup("full-reconcile")
	r.NotNil(flag)
	oldChanged := flag.Changed
	t.Cleanup(func() { importEmlxFullReconcile = old; flag.Changed = oldChanged })
	r.NoError(importEmlxCmd.Flags().Parse([]string{"--full-reconcile"}))
	a.True(importEmlxFullReconcile)
	var output bytes.Buffer
	printImportStats(&output, importer.EmlxImportSummary{MessagesProcessed: 3, MessagesAdded: 1, MessagesSkipped: 2, FilesUnchanged: 2})
	a.Contains(output.String(), "Unchanged:      2 files (content reads avoided)")
	a.Contains(output.String(), "Processed:      3 messages")
}
