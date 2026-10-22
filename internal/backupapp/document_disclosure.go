package backupapp

import "go.kenn.io/kit/backup"

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
