package backupapp

import (
	"context"
	"fmt"

	"go.kenn.io/kit/backup"
)

const (
	documentDisclosureName   = "document-derivatives"
	documentDisclosureFormat = "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true"
)

func ManifestContainsDocumentPlaintext(manifest *backup.Manifest) bool {
	if manifest == nil {
		return false
	}
	for _, artifact := range manifest.Auxiliary {
		if artifact.Name == documentDisclosureName && artifact.Format == documentDisclosureFormat {
			return true
		}
	}
	return false
}

// DocumentAuxiliaryTarget accepts historical document text that the archive no
// longer stores. Kit verifies the artifact bytes before handing them here.
type DocumentAuxiliaryTarget struct{}

func (DocumentAuxiliaryTarget) StageAuxiliary(_ context.Context, artifacts []backup.RestoredAuxiliary) (backup.AuxiliaryRestore, error) {
	for _, artifact := range artifacts {
		if artifact.Name != documentDisclosureName || artifact.Format != documentDisclosureFormat {
			return nil, fmt.Errorf("unsupported backup auxiliary artifact %q with format %q", artifact.Name, artifact.Format)
		}
	}
	return discardedDocumentAuxiliary{}, nil
}

type discardedDocumentAuxiliary struct{}

func (discardedDocumentAuxiliary) Commit(context.Context) error   { return nil }
func (discardedDocumentAuxiliary) Rollback(context.Context) error { return nil }
