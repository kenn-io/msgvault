package backupapp_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

func TestManifestContainsDocumentPlaintext(t *testing.T) {
	assert.False(t, backupapp.ManifestContainsDocumentPlaintext(nil))
	assert.False(t, backupapp.ManifestContainsDocumentPlaintext(&backup.Manifest{}))
	manifest := &backup.Manifest{Auxiliary: []backup.ManifestAuxiliary{{Name: "document-derivatives", Format: "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true"}}}
	assert.True(t, backupapp.ManifestContainsDocumentPlaintext(manifest))
}

func TestDocumentAuxiliaryTargetRejectsUndisclosedArtifacts(t *testing.T) {
	target := backupapp.DocumentAuxiliaryTarget{}
	_, err := target.StageAuxiliary(context.Background(), []backup.RestoredAuxiliary{{
		Name: "unexpected", Format: "application/json", Data: []byte(`{}`),
	}})
	require.ErrorContains(t, err, "unsupported")
}

func TestDocumentAuxiliaryTargetAcceptsLegacyDisclosureSchema(t *testing.T) {
	target := backupapp.DocumentAuxiliaryTarget{}
	staged, err := target.StageAuxiliary(t.Context(), []backup.RestoredAuxiliary{{
		Name:   "document-derivatives",
		Format: "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true",
		Data:   []byte(`{"schema_version":1,"contains_normalized_attachment_plaintext":true,"chunks":1,"characters":4}`),
	}})
	require.NoError(t, err)
	require.NoError(t, staged.Commit(t.Context()))
}
