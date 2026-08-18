package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeMessageRawBoundedStopsAtLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err := writer.Write([]byte(strings.Repeat("x", 128)))
	require.NoError(err)
	require.NoError(writer.Close())

	raw, bytesRead, err := decodeMessageRawBounded(
		compressed.Bytes(), sql.NullString{String: "zlib", Valid: true}, 64,
	)
	require.ErrorIs(err, errAttachmentRoleRepairMessageTooLarge)
	assert.Nil(raw)
	assert.Equal(int64(65), bytesRead)
}
