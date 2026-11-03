//go:build windows

package metawebhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Native Windows must exercise startup and publication: directory handles
// cannot be flushed with os.File.Sync on that platform.
func TestSpoolWindowsAcceptAndReopen(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	root := t.TempDir()
	spool, err := OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	body := []byte(`{"object":"page","entry":[]}`)
	id, duplicate, err := spool.Accept("9", body)
	require.NoError(err)
	assert.False(duplicate)
	reopened, err := OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	got, err := reopened.Body("9", id)
	require.NoError(err)
	assert.Equal(body, got)
	due, err := reopened.Due(1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Equal(id, due[0].ID)
}
