//go:build !linux && !darwin

package importer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emlx"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportEmlxUnsupportedIdentityUsesRealColdPath(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": email.NewMessage().From("sender@example.test").Body("synthetic fallback").Bytes()})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	io := defaultEmlxImportIO(opts)
	parse := io.parse
	reads := 0
	io.parse = func(path string, byteLimit int64) (*emlx.Message, error) { reads++; return parse(path, byteLimit) }
	first, err := importEmlxDir(t.Context(), st, root, opts, io)
	r.NoError(err)
	r.False(first.HardErrors)
	a.Equal(int64(1), first.MessagesAdded)
	second, err := importEmlxDir(t.Context(), st, root, opts, io)
	r.NoError(err)
	a.False(second.HardErrors)
	a.Zero(second.FilesUnchanged)
	a.Equal(2, reads)
}
