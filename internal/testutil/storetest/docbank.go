package storetest

import (
	"fmt"
	"strings"

	"github.com/stretchr/testify/require"
)

// DocumentAttachment describes a mirrored attachment for a search fixture.
type DocumentAttachment struct {
	MIMEType  string
	VersionID string
}

// SeedDocumentAttachments inserts mirrored attachments in one transaction.
func (f *Fixture) SeedDocumentAttachments(messageID int64, documents []DocumentAttachment) {
	f.T.Helper()
	tx, err := f.Store.DB().BeginTx(f.T.Context(), nil)
	require.NoError(f.T, err)
	defer func() { _ = tx.Rollback() }()
	for i, document := range documents {
		hash := fmt.Sprintf("%064x", 0xabcdef+i)
		_, err := tx.Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,content_hash,storage_path,attachment_state) VALUES (?,'file',?,?,'stored')`), messageID, strings.ToUpper(hash), hash[:2]+"/"+hash)
		require.NoError(f.T, err)
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO docbank_attachment_deliveries(destination_key,content_hash,upload_name,mime_type,state,node_id,version_id,endpoint,folder) VALUES ('attachments',?,'file',?,'delivered',7,?,'','')`), hash, document.MIMEType, document.VersionID)
		require.NoError(f.T, err)
	}
	require.NoError(f.T, tx.Commit())
}
