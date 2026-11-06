package email

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Maildir creates an empty Maildir folder for import tests.
func Maildir(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"cur", "new", "tmp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
}
