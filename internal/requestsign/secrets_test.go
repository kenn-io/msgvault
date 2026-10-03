package requestsign

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestSigningSecretIsIndependentPrivateAndBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "secret")
	want := bytes.Repeat([]byte{0x32}, 64)
	requirements.NoError(fileutil.SecureWriteFile(path, []byte(base64.StdEncoding.EncodeToString(want)+"\n"), 0o600))
	got, err := ReadSigningSecret(path)
	requirements.NoError(err)
	assertions.Equal(want, got)
	for _, data := range []string{"not base64", base64.StdEncoding.EncodeToString(want[:32]), strings.Repeat("a", 4097)} {
		requirements.NoError(fileutil.SecureWriteFile(path, []byte(data), 0o600))
		_, err = ReadSigningSecret(path)
		requirements.Error(err)
	}
	if runtime.GOOS != "windows" {
		requirements.NoError(fileutil.SecureWriteFile(path, []byte(base64.StdEncoding.EncodeToString(want)), 0o600))
		requirements.NoError(os.Chmod(path, 0o644))
		_, err = ReadSigningSecret(path)
		requirements.Error(err)
		requirements.NoError(os.Chmod(path, 0o600))
		link := filepath.Join(t.TempDir(), "symlink")
		requirements.NoError(os.Symlink(path, link))
		_, err = ReadSigningSecret(link)
		requirements.Error(err)
	}
}
