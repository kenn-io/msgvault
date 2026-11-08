package messageimport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validRequestJSON = `{
  "source": {
    "identifier": " feeds ",
    "display_name": " Feeds ",
    "account_email": " USER@example.com "
  },
  "message": {
    "external_id": " entry-42 ",
    "message_type": "feed_item",
    "conversation": {"external_id": " feed-7 ", "title": " Example Project releases "},
    "sender": {"name": " Example Project ", "url": " https://Example.com/blog "},
    "title": " Example Project 1.2.0 ",
    "url": " https://example.com/releases/1.2.0 ",
    "sent_at": "2026-11-07T10:00:00-05:00",
    "body_markdown": "## Changes\n\n- Faster widgetsync for everyone.",
    "metadata": {"entry_hash": "synthetic-hash", "big": 12345678901234567890, "nested": {"starred": true}}
  }
}`

func validRequest(t *testing.T) Request {
	t.Helper()
	req, err := DecodeRequest(strings.NewReader(validRequestJSON), MaxRequestBytes)
	require.NoError(t, err)
	return req
}

func TestDecodeRequestPreservesLargeMetadataNumbers(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	normalized, err := validRequest(t).Normalize()
	require.NoError(err)
	snapshot, err := BuildSnapshot(normalized)
	require.NoError(err)

	assert.Contains(t, string(snapshot.Raw), `"big":12345678901234567890`)
	assert.Contains(t, string(snapshot.Metadata), `"big":12345678901234567890`)
}

func TestDecodeRequestRejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"empty":                  "  ",
		"not json":               "message",
		"trailing value":         validRequestJSON + ` {}`,
		"unknown top-level":      strings.Replace(validRequestJSON, `"source": {`, `"extra": 1, "source": {`, 1),
		"unknown message member": strings.Replace(validRequestJSON, `"message_type"`, `"kind": "x", "message_type"`, 1),
		"unknown sender member":  strings.Replace(validRequestJSON, `"sender": {`, `"sender": {"handle": "x", `, 1),
		"invalid utf8":           strings.Replace(validRequestJSON, "Feeds", "Fe\xffeds", 1),
		"wrong type":             strings.Replace(validRequestJSON, `"message_type": "feed_item"`, `"message_type": 7`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
			require.ErrorIs(t, err, ErrMalformedRequest)
		})
	}
}

func TestDecodeRequestEnforcesBodyLimit(t *testing.T) {
	t.Parallel()
	limit := int64(len(validRequestJSON))

	_, err := DecodeRequest(strings.NewReader(validRequestJSON), limit)
	require.NoError(t, err, "a body of exactly the limit is accepted")

	_, err = DecodeRequest(bytes.NewReader(append([]byte(validRequestJSON), ' ')), limit)
	require.ErrorIs(t, err, ErrRequestTooLarge)
}
