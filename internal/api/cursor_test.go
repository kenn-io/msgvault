package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// changesAfter is the position just after a row, which is what every page that
// returned rows publishes. cursorAfterID reads the id half back out, reporting
// whether the position carries one at all.
func changesAfter(at time.Time, id int64) store.ChangedMessagesCursor {
	return store.ChangedMessagesAfter(at, id)
}

func cursorAfterID(t *testing.T, got changesPosition) int64 {
	t.Helper()
	id, ok := got.cursor.AfterID()
	require.True(t, ok, "the decoded position must carry an id tiebreak")
	return id
}

// This file is the only place allowed to know what a change-feed cursor is made
// of. Every other test treats it as the opaque string the API publishes.

// testArchiveUID stands in for the durable identity of the archive these
// cursors belong to. The codec never interprets it, so its shape does not
// matter here — only that encode writes it and decode insists on it.
const testArchiveUID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

// TestChangesCursorRoundTripsSubSecondPrecision is the loop guard at codec
// level: the watermark carries the database's full sub-second resolution, and a
// cursor that loses it resumes below the page it was handed and re-delivers that
// page on every poll, forever.
func TestChangesCursorRoundTripsSubSecondPrecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	at := time.Date(2026, 7, 26, 10, 0, 0, 731123456, time.UTC)
	require.NotZero(at.Nanosecond(), "this test is meaningless without a sub-second part")

	got, err := decodeChangesCursor(encodeChangesCursor(testArchiveUID, changesAfter(at, 918)))
	require.NoError(err, "decode the cursor just encoded")
	require.NoError(got.boundTo(testArchiveUID), "the cursor must belong to the archive that issued it")
	assert.True(got.cursor.At().Equal(at), "watermark: want %s, got %s", at, got.cursor.At())
	assert.Equal(at.Nanosecond(), got.cursor.At().Nanosecond(),
		"nanoseconds must survive the round trip")
	assert.Equal(int64(918), cursorAfterID(t, got), "id tiebreak")
}

// TestChangesCursorRoundTripsInUTC pins that the wire form is UTC whatever the
// caller hands in, so two cursors for the same instant are the same string.
func TestChangesCursorRoundTripsInUTC(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	zone := time.FixedZone("UTC-7", -7*60*60)
	at := time.Date(2026, 7, 26, 3, 0, 0, 731123456, zone)

	token := encodeChangesCursor(testArchiveUID, changesAfter(at, 42))
	got, err := decodeChangesCursor(token)
	require.NoError(err, "decode")
	require.NoError(got.boundTo(testArchiveUID), "the cursor must belong to the archive that issued it")
	assert.True(got.cursor.At().Equal(at), "instant: want %s, got %s", at, got.cursor.At())
	assert.Equal(time.UTC, got.cursor.At().Location(), "the decoded watermark must be UTC")
	assert.Equal(int64(42), cursorAfterID(t, got), "id tiebreak")
	assert.Equal(encodeChangesCursor(testArchiveUID, changesAfter(at.UTC(), 42)), token,
		"the same instant in two locations must encode to the same cursor")
}

// TestChangesCursorRoundTripsTheStartOfTheArchive covers the position a caller
// who has never polled holds: the zero time with no tiebreak. It is a real
// cursor, published on the first page, and it has to come back as itself.
func TestChangesCursorRoundTripsTheStartOfTheArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	token := encodeChangesCursor(testArchiveUID, store.ChangedMessagesCursor{})
	require.NotEmpty(token, "a cursor is always publishable, the start of the archive included")

	got, err := decodeChangesCursor(token)
	require.NoError(err, "decode")
	require.NoError(got.boundTo(testArchiveUID), "the cursor must belong to the archive that issued it")
	assert.True(got.cursor.At().IsZero(),
		"the start of the archive is the zero time, got %s", got.cursor.At())
	_, hasID := got.cursor.AfterID()
	assert.False(hasID, "the start of the archive carries no id tiebreak")
}

// TestChangesCursorRoundTripsTheStartOfAnInstant covers the OTHER position the
// server publishes: the start of an instant, which the future-cursor clamp hands
// back. It has to survive the round trip as the start of that instant rather
// than as a position after some row in it, because every int64 is a legal
// message id — no tiebreak value would let the rows at 0 and below through, and
// the clamp published 0 until this was fixed.
func TestChangesCursorRoundTripsTheStartOfAnInstant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	at := time.Date(2026, 7, 26, 10, 0, 0, 731123456, time.UTC)
	token := encodeChangesCursor(testArchiveUID, store.ChangedMessagesFrom(at))

	got, err := decodeChangesCursor(token)
	require.NoError(err, "decode")
	require.NoError(got.boundTo(testArchiveUID), "the cursor must belong to the archive that issued it")
	assert.True(got.cursor.At().Equal(at), "instant: want %s, got %s", at, got.cursor.At())
	id, hasID := got.cursor.AfterID()
	assert.Falsef(hasID,
		"a position at the start of an instant must not come back carrying a "+
			"tiebreak (got %d): the store would then skip every row stamped there "+
			"at or below it", id)
	assert.NotEqual(encodeChangesCursor(testArchiveUID, changesAfter(at, 0)), token,
		"and it must not encode to the same token as the position after id 0, "+
			"which is the position that dropped the rows at id 0 and below")
}

// TestChangesCursorRejectsAMalformedToken: an UNREADABLE cursor is reported as
// one. Reading it as the zero value instead would turn a client bug into a
// silent re-delivery of the entire archive.
//
// Named for what it checks. It used to be called RejectsWhatItDidNotIssue, which
// promised a guarantee this server cannot make and this body never tested: every
// case below is a token that does not decode, and a token that DOES decode is
// accepted whether or not this server minted it — see
// TestChangesCursorAcceptsAFabricatedTokenForItsOwnArchive.
func TestChangesCursorRejectsAMalformedToken(t *testing.T) {
	v1 := func(payload string) string {
		return "1." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	}

	tests := []struct {
		name string
		raw  string
	}{
		{"no version prefix at all", base64.RawURLEncoding.EncodeToString(
			[]byte(`{"t":"2026-07-26T10:00:00Z","i":1}`))},
		{"a version prefix that is not a number", "v1.eyJ0IjoieCJ9"},
		{"a signed version prefix", "+1.eyJ0IjoieCJ9"},
		{"not base64url after the version", "1.not a cursor!!"},
		{"base64 of something that is not JSON", v1("nowhere near JSON")},
		{"no watermark", v1(`{"i":1}`)},
		{"an unreadable watermark", v1(`{"t":"yesterday","i":1}`)},
		{"an id that is not a number", v1(`{"t":"2026-07-26T10:00:00Z","i":"1"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeChangesCursor(tc.raw)
			require.Error(t, err, "decode %q", tc.raw)

			var pe *paramError
			require.ErrorAs(t, err, &pe,
				"the failure must name the parameter, so the response carries invalid_cursor")
			assert.Equal(t, "cursor", pe.param, "parameter")
		})
	}
}

// TestChangesCursorAcceptsAFabricatedTokenForItsOwnArchive pins a DECISION, so
// that the next reader does not mistake it for an oversight and "fix" it.
//
// The cursor is not signed and not authenticated. The server has no secret to
// sign with, and a forged cursor buys its holder nothing: it moves that caller's
// own position in that caller's own feed and reaches no message the caller could
// not already request. So a hand-built, never-issued, current-format token
// carrying a position no page ever published is accepted, exactly as if the
// server had minted it — including a position in the future and an id no row
// has. Nothing here is a whole-archive check: what the server enforces is that
// the token decodes, that its format version is one it speaks, and that it names
// THIS archive.
//
// If tamper-evidence is ever wanted, it needs a key and a new cursor format, not
// a tightening of this path.
func TestChangesCursorAcceptsAFabricatedTokenForItsOwnArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Built by hand from the published shape, not by encodeChangesCursor: the
	// point is a token this server never issued.
	fabricated := "1." + base64.RawURLEncoding.EncodeToString([]byte(
		`{"t":"2999-12-31T23:59:59.999999Z","i":4294967296,"a":"`+testArchiveUID+`"}`))

	got, err := decodeChangesCursor(fabricated)
	require.NoError(err,
		"a well-formed current-format token must be readable whoever built it")
	require.NoError(got.boundTo(testArchiveUID),
		"and accepted, because it names this archive; the token is not authenticated")
	assert.Equal(int64(4294967296), cursorAfterID(t, got),
		"the fabricated position is honoured as given, not sanitised")
	assert.Equal(2999, got.cursor.At().Year(),
		"including a watermark no page ever published")
}

// TestChangesCursorIsBoundToTheArchiveThatIssuedIt is the codec half of the
// archive binding. The position a cursor carries is a watermark plus an
// archive-LOCAL message id, so it is meaningful in exactly one archive; pointed
// at a different --db, or at an archive rebuilt from scratch, it would resume
// the walk somewhere unrelated and silently omit everything before that point.
//
// The empty archive covers the token shape that existed before the binding and
// anything hand-built without one: an unnamed archive is not a wildcard.
func TestChangesCursorIsBoundToTheArchiveThatIssuedIt(t *testing.T) {
	const otherArchive = "ffeeddccbbaa99887766554433221100"
	at := time.Date(2026, 7, 26, 10, 0, 0, 731123456, time.UTC)

	for name, issuedBy := range map[string]string{
		"a cursor from another archive": otherArchive,
		"a cursor naming no archive":    "",
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			got, err := decodeChangesCursor(encodeChangesCursor(issuedBy, changesAfter(at, 918)))
			require.NoError(err, "the token's shape is fine; only where it belongs is wrong")

			err = got.boundTo(testArchiveUID)
			require.Errorf(err, "a position from %q must not be honoured here", issuedBy)
			var pe *paramError
			require.ErrorAs(err, &pe,
				"the failure must name the parameter, so the response carries invalid_cursor")
			assert.Equal("cursor", pe.param, "parameter")
			assert.Contains(err.Error(), "different archive", "the message must name the cause")
			assert.Contains(err.Error(), "from the beginning", "and the repair")
		})
	}
}

// TestChangesCursorForTheStartOfTheArchiveBelongsToEveryArchive: an absent
// cursor names no position, so there is nothing to misplace and nothing to
// bind. Binding it would make a first-ever poll a 400.
func TestChangesCursorForTheStartOfTheArchiveBelongsToEveryArchive(t *testing.T) {
	var absent changesPosition
	require.NoError(t, absent.boundTo(testArchiveUID),
		"the start of the archive must be accepted by the archive it is aimed at")
	require.NoError(t, absent.boundTo("some other archive entirely"),
		"and by any other, because it names no position in any of them")
}

// TestChangesCursorFromAnotherVersionSaysHowToRecover: an operator holding a
// cursor this build cannot read has exactly one move, and guessing it from
// "invalid" is not reasonable.
func TestChangesCursorFromAnotherVersionSaysHowToRecover(t *testing.T) {
	raw := "2." + base64.RawURLEncoding.EncodeToString([]byte(`{"t":"2026-07-26T10:00:00Z","i":1}`))

	_, err := decodeChangesCursor(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from the beginning",
		"the message must name the repair: the sync restarts from the beginning")
	assert.Contains(t, err.Error(), "2", "and it must report the version it found")
}

// TestChangesCursorFromAnotherVersionIsRecognisedWhateverItWraps is why the
// version sits outside the encoded part. A later format is free to stop being
// base64url-wrapped JSON — signed, binary, anything — and this server still has
// to tell its holder "wrong server version, restart the sync" rather than
// "corrupt". A version buried inside the envelope cannot: it is unreadable until
// the envelope has already been agreed on, so any format change beyond swapping
// fields degrades to indistinguishable-from-garbage.
func TestChangesCursorFromAnotherVersionIsRecognisedWhateverItWraps(t *testing.T) {
	tests := map[string]string{
		"a payload that is not base64url at all": "2.\x00\x01binary, signed, who knows",
		"a payload this server cannot even see":  "2.",
		"a far future version":                   "37." + base64.RawURLEncoding.EncodeToString([]byte("a signed token, say")),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeChangesCursor(raw)
			require.Error(t, err, "decode %q", raw)
			assert.Contains(t, err.Error(), "from the beginning",
				"a cursor whose version this server does not speak must be reported as "+
					"that, not as corruption: what follows the version is by definition "+
					"not this server's to parse")
		})
	}
}

// TestChangesCursorNeedsNoEscapingInAQueryString is why the token uses the
// URL-safe alphabet with no padding, and why the version prefix is digits and a
// `.` rather than anything more decorative: all of it is unreserved. A cursor
// that has to be escaped is one a consumer can corrupt by pasting it into a URL.
func TestChangesCursorNeedsNoEscapingInAQueryString(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	token := encodeChangesCursor(testArchiveUID,
		changesAfter(time.Date(2026, 7, 26, 10, 0, 0, 731123456, time.UTC), 918))
	assert.Equal(token, url.QueryEscape(token), "the token must need no escaping")

	target := "/api/v1/messages/changes?cursor=" + token
	parsed, err := url.Parse(target)
	require.NoError(err, "parse %q", target)
	assert.Equal(token, parsed.RawQuery[len("cursor="):], "the raw query must carry the token verbatim")
	assert.Equal(token, parsed.Query().Get("cursor"), "and it must survive query parsing")

	got, err := decodeChangesCursor(parsed.Query().Get("cursor"))
	require.NoError(err, "decode the cursor after the query-string round trip")
	require.NoError(got.boundTo(testArchiveUID), "the cursor must belong to the archive that issued it")
	assert.True(got.cursor.At().Equal(time.Date(2026, 7, 26, 10, 0, 0, 731123456, time.UTC)),
		"watermark")
	assert.Equal(int64(918), cursorAfterID(t, got), "id tiebreak")
}

// TestChangesCursorParamReadsAnEmptyValueAsAbsent pins the surprising half of
// the request contract: `?cursor=` is absent, not a parse failure, exactly as
// every other empty query value in this API is read.
func TestChangesCursorParamReadsAnEmptyValueAsAbsent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	for _, target := range []string{"/x", "/x?cursor=", "/x?cursor=&limit=10"} {
		u, err := url.Parse(target)
		require.NoError(err, "parse %q", target)
		position, err := queryChangesCursor(&http.Request{URL: u})
		require.NoErrorf(err, "%q must not be a parse failure", target)
		require.NoErrorf(position.boundTo(testArchiveUID),
			"%q names no position, so it belongs to every archive", target)
		assert.Truef(position.cursor.At().IsZero(),
			"%q must start from the beginning of the archive", target)
		_, hasID := position.cursor.AfterID()
		assert.Falsef(hasID, "%q must carry no tiebreak", target)
	}
}
