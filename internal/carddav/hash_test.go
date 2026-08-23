package carddav

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSemanticHashIgnoresOnlyServerOwnedProperties(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	left := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Alice Example\r\nREV:20260101T000000Z\r\nPRODID:-//one//EN\r\nEND:VCARD\r\n")
	right := []byte("BEGIN:VCARD\nVERSION:4.0\nUID:remote-1\nFN:Alice Example\nREV:20260819T120000Z\nPRODID:-//two//EN\nEND:VCARD\n")
	changed := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Bob Example\r\nREV:20260819T120000Z\r\nEND:VCARD\r\n")

	leftHash, err := SemanticHash(left)
	require.NoError(err)
	rightHash, err := SemanticHash(right)
	require.NoError(err)
	changedHash, err := SemanticHash(changed)
	require.NoError(err)

	assert.Equal(leftHash, rightHash)
	assert.NotEqual(leftHash, changedHash)
}

func TestSemanticHashRejectsMalformedVCard(t *testing.T) {
	_, err := SemanticHash([]byte("not a vCard"))
	require.Error(t, err)
}

func TestSemanticHashIgnoresPropertyOrderButPreservesDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	left := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Alice Example\r\nEMAIL;TYPE=work:alice@example.test\r\nEMAIL;TYPE=home:alice@home.test\r\nEND:VCARD\r\n")
	right := []byte("BEGIN:VCARD\r\nEMAIL;TYPE=home:alice@home.test\r\nFN:Alice Example\r\nVERSION:4.0\r\nEMAIL;TYPE=work:alice@example.test\r\nUID:remote-1\r\nEND:VCARD\r\n")
	missingDuplicate := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Alice Example\r\nEMAIL;TYPE=work:alice@example.test\r\nEND:VCARD\r\n")

	leftHash, err := SemanticHash(left)
	require.NoError(err)
	rightHash, err := SemanticHash(right)
	require.NoError(err)
	missingHash, err := SemanticHash(missingDuplicate)
	require.NoError(err)

	assert.Equal(leftHash, rightHash)
	assert.NotEqual(leftHash, missingHash)
}

func TestSemanticHashCanonicalizesKnownTextAndURIValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	left := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Alice Example\r\nNOTE:Line one\\nLine two\r\nURL;VALUE=URI:HTTPS://example.test/alice\r\nEND:VCARD\r\n")
	right := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN;VALUE=text:Alice Example\r\nNOTE:Line one\\NLine two\r\nURL:https://example.test/alice\r\nEND:VCARD\r\n")
	changed := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-1\r\nFN:Alice Example\r\nNOTE:Line one\\nDifferent\r\nURL:https://example.test/alice\r\nEND:VCARD\r\n")

	leftHash, err := SemanticHash(left)
	require.NoError(err)
	rightHash, err := SemanticHash(right)
	require.NoError(err)
	changedHash, err := SemanticHash(changed)
	require.NoError(err)

	assert.Equal(leftHash, rightHash)
	assert.NotEqual(leftHash, changedHash)
}
