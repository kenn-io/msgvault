//go:build !linux && !darwin

package emlx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmlxUnsupportedIdentityStaysCold(t *testing.T) {
	p := filepath.Join(t.TempDir(), "1.emlx")
	require.NoError(t, os.WriteFile(p, []byte("synthetic"), 0600))
	_, eligible, err := Fingerprint(t.Context(), p)
	require.NoError(t, err)
	assert.False(t, eligible)
}
