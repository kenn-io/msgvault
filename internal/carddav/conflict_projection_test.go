package carddav

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectConflictContactAllowListsNormalizesAndBoundsFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	long := strings.Repeat("界", 90)
	raw := "BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"UID:private-uid\r\nURL:https://dav.invalid/private\r\n" +
		"NOTE:Authorization: Bearer private-token\r\nX-API-KEY:private-key\r\n" +
		"FN:\t Alice\u0001   Example \r\nFN:Ignored Second Name\r\n" +
		"EMAIL:MAILTO:alice@example.test\r\nEMAIL:alice@example.test\r\n" +
		"EMAIL:" + long + "\r\n" +
		"EMAIL:e2@example.test\r\nEMAIL:e3@example.test\r\nEMAIL:e4@example.test\r\n" +
		"EMAIL:e5@example.test\r\nEMAIL:e6@example.test\r\nEMAIL:e7@example.test\r\nEMAIL:e8@example.test\r\n" +
		"TEL:TEL:+1  555  0100\r\nTEL:+1 555 0100\r\nEND:VCARD\r\n"

	got := projectConflictContact([]byte(raw), false)
	require.Equal(ConflictSidePresent, got.State)
	assert.Equal("Alice Example", got.DisplayName)
	require.Len(got.Emails, 8)
	assert.Equal("alice@example.test", got.Emails[0])
	assert.Equal([]string{"+1 555 0100"}, got.Phones)
	assert.True(got.Truncated)
	assert.LessOrEqual(len(got.Emails[1]), 256)
	assert.True(utf8.ValidString(got.Emails[1]))

	encoded, err := json.Marshal(got)
	require.NoError(err)
	text := string(encoded)
	for _, forbidden := range []string{"private-uid", "dav.invalid", "Authorization", "private-token", "private-key", "X-API-KEY", "Ignored Second Name"} {
		assert.NotContains(text, forbidden)
	}
}

func TestProjectConflictContactTombstoneAndMalformedAreFixed(t *testing.T) {
	assert := assert.New(t)
	deleted := projectConflictContact([]byte("credential=private-secret"), true)
	assert.Equal(ContactSummary{State: ConflictSideDeleted, Emails: []string{}, Phones: []string{}}, deleted)

	unavailable := projectConflictContact([]byte("not a vcard private-parser-marker"), false)
	assert.Equal(ContactSummary{State: ConflictSideUnavailable, Emails: []string{}, Phones: []string{}}, unavailable)
	encoded, err := json.Marshal(unavailable)
	require.NoError(t, err)
	assert.NotContains(string(encoded), "private-parser-marker")
}

func TestProjectConflictContactPreservesWhitespaceSeparatorsAndExactDeduplication(t *testing.T) {
	assert := assert.New(t)
	prefix := strings.Repeat("a", 256)
	raw := "BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN:Alice\\nExample\tPerson\r\n" +
		"EMAIL:" + prefix + "x@example.test\r\n" +
		"EMAIL:" + prefix + "y@example.test\r\n" +
		"EMAIL:" + prefix + "x@example.test\r\n" +
		"END:VCARD\r\n"

	got := projectConflictContact([]byte(raw), false)
	assert.Equal("Alice Example Person", got.DisplayName)
	require.Len(t, got.Emails, 2, "distinct normalized values sharing a clipped prefix are not duplicates")
	assert.Equal(got.Emails[0], got.Emails[1])
	assert.Len(got.Emails[0], 256)
	assert.True(got.Truncated)
}

func TestProjectConflictContactIgnoresLaterFNWithoutReportingTruncation(t *testing.T) {
	raw := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:First Name\r\nFN:" +
		strings.Repeat("private-later-name", 40) + "\r\nEND:VCARD\r\n"
	got := projectConflictContact([]byte(raw), false)
	assert.Equal(t, "First Name", got.DisplayName)
	assert.False(t, got.Truncated)
}
