package backupapp_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

func TestManifestContainsDocumentPlaintext(t *testing.T) {
	assert.False(t, backupapp.ManifestContainsDocumentPlaintext(nil))
	assert.False(t, backupapp.ManifestContainsDocumentPlaintext(&backup.Manifest{}))
	manifest := &backup.Manifest{Auxiliary: []backup.ManifestAuxiliary{{Name: "document-derivatives", Format: "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true"}}}
	assert.True(t, backupapp.ManifestContainsDocumentPlaintext(manifest))
}
