//go:build windows

package export

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/mime"
)

func TestStoreAttachmentFileDurableWindowsRejectsFinalReparsePoint(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	content := []byte("reparse target bytes")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	hashDir := filepath.Join(dir, hash[:2])
	require.NoError(os.MkdirAll(hashDir, 0o700))
	target := filepath.Join(dir, "outside")
	require.NoError(os.WriteFile(target, content, 0o600))
	canonical := filepath.Join(hashDir, hash)
	// Native Windows CI must support creating the reparse point; silently
	// skipping would leave the final-component security property untested.
	require.NoError(os.Symlink(target, canonical))

	_, err := StoreAttachmentFileDurable(dir, &mime.Attachment{
		Content: content, ContentHash: hash,
	})
	require.ErrorIs(err, packstore.ErrContentMismatch)
}
