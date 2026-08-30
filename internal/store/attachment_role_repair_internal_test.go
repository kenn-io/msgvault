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

func TestDecodeMessageRawHeaderBoundedStopsAfterHeaders(t *testing.T) {
	require := require.New(t)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	header := "From: sender@example.test\r\nSubject: bounded\r\n\r\n"
	_, err := writer.Write([]byte(header + strings.Repeat("body", 1_000)))
	require.NoError(err)
	require.NoError(writer.Close())

	raw, err := decodeMessageRawHeaderBounded(
		compressed.Bytes(), sql.NullString{String: "zlib", Valid: true}, 128,
	)

	require.NoError(err)
	assert.Equal(t, []byte(header), raw)
}

func TestDecodeMessageRawHeaderBoundedRejectsOversizedHeaders(t *testing.T) {
	raw, err := decodeMessageRawHeaderBounded(
		[]byte("From: "+strings.Repeat("x", 128)), sql.NullString{}, 64,
	)

	require.ErrorIs(t, err, errSenderRepairHeaderTooLarge)
	assert.Nil(t, raw)
}
