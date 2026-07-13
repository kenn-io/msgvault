package contentverify_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/contentverify"
)

func TestReadCloserVerification(t *testing.T) {
	content := []byte("verified attachment response")
	corrupt := bytes.Clone(content)
	corrupt[len(corrupt)-1] ^= 0xff
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	tests := []struct {
		name    string
		content []byte
		wantErr error
	}{
		{name: "matching", content: content},
		{name: "same length mismatch", content: corrupt, wantErr: contentverify.ErrMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			reader, err := contentverify.NewReadCloser(io.NopCloser(bytes.NewReader(tt.content)), hash)
			require.NoError(err)
			got, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if tt.wantErr != nil {
				require.ErrorIs(readErr, tt.wantErr)
				require.ErrorIs(closeErr, tt.wantErr)
				return
			}
			require.NoError(errors.Join(readErr, closeErr))
			assert.Equal(t, content, got)
		})
	}
}

func TestReadCloserRejectsEarlyClose(t *testing.T) {
	content := []byte("partial attachment")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	reader, err := contentverify.NewReadCloser(io.NopCloser(bytes.NewReader(content)), hash)
	require.NoError(t, err)
	_, err = reader.Read(make([]byte, 1))
	require.NoError(t, err)
	require.ErrorIs(t, reader.Close(), contentverify.ErrVerificationIncomplete)
}

func TestVerifyBytesAcceptsUppercaseHash(t *testing.T) {
	content := []byte("buffered attachment")
	hash := fmt.Sprintf("%X", sha256.Sum256(content))
	require.NoError(t, contentverify.VerifyBytes(content, hash))
	require.ErrorIs(t, contentverify.VerifyBytes([]byte("same sized corruption"), hash), contentverify.ErrMismatch)
	require.Error(t, contentverify.VerifyBytes(content, strings.Repeat("z", 64)))
}
