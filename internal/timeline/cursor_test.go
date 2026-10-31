package timeline

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func cursorBinding() CursorBinding {
	return CursorBinding{ArchiveUID: "synthetic-archive", Request: Request{Mode: ModeDay, Format: "json", Scope: Scope{Owner: true}}, Window: Window{From: "2026-10-21", To: "2026-10-22"}, Timezone: "UTC", Now: time.Date(2026, 10, 22, 13, 30, 0, 0, time.UTC)}
}
func TestCursorRoundTrip(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	b := cursorBinding()
	encoded, err := EncodeCursor(b, map[string]string{"history": "2026-10-22T13:30:00Z|message|msg:31"})
	require.NoError(err)
	b.Now = b.Now.Add(time.Hour)
	b.Request.Budget = 4096
	b.Request.Cursor = "ignored"
	c, err := DecodeCursor(encoded, b)
	require.NoError(err)
	assert.Equal(int64(1792675800), c.Now.Unix())
	assert.Equal("2026-10-22T13:30:00Z|message|msg:31", c.Positions["history"])
	b.Request.Mode = ""
	c, err = DecodeCursor(encoded, b)
	require.NoError(err)
	assert.Len(c.Positions, 1)
	// Canonicalize unordered set-valued filters without changing the request.
	b.Request.Tiers = []int{2, 1, 2}
	b.Request.Scope.OpenTools = []string{"b", "a"}
	b.Request.Scope.PersonIDs = []int64{8, 7}
	encoded, err = EncodeCursor(b, nil)
	require.NoError(err)
	b.Request.Tiers = []int{1, 2}
	b.Request.Scope.OpenTools = []string{"a", "b"}
	b.Request.Scope.PersonIDs = []int64{7, 8}
	_, err = DecodeCursor(encoded, b)
	require.NoError(err)
}
func TestCursorPinsFractionalNow(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	b := cursorBinding()
	b.Now = b.Now.Add(123456789 * time.Nanosecond)
	want := b.Now
	var windowNow time.Time
	b.WindowAt = func(now time.Time) (Window, error) {
		windowNow = now
		return b.Window, nil
	}
	encoded, err := EncodeCursor(b, map[string]string{"history": "key"})
	require.NoError(err)
	b.Now = b.Now.Add(time.Hour)
	c, err := DecodeCursor(encoded, b)
	require.NoError(err)
	assert.Equal(want, c.Now)
	assert.Equal(want, windowNow)
}
func TestCursorRejectsInvalidNanoseconds(t *testing.T) {
	require := requirepkg.New(t)
	b := cursorBinding()
	encoded, err := EncodeCursor(b, nil)
	require.NoError(err)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(err)
	var fields map[string]any
	require.NoError(json.Unmarshal(raw, &fields))
	for _, nanos := range []int{-1, 1000000000} {
		fields["ns"] = nanos
		raw, err := json.Marshal(fields)
		require.NoError(err)
		_, err = DecodeCursor(base64.RawURLEncoding.EncodeToString(raw), b)
		require.ErrorIs(err, ErrCursorExpired)
	}
}
func TestCursorRejectsChangedScope(t *testing.T) {
	b := cursorBinding()
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.Request.Scope = Scope{PersonIDs: []int64{7}, OpenTools: []string{"get_message"}}
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorRejectsChangedFormat(t *testing.T) {
	b := cursorBinding()
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.Request.Format = "markdown"
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorRejectsOtherArchive(t *testing.T) {
	b := cursorBinding()
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.ArchiveUID = "other-archive"
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorSizeLimit(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	b := cursorBinding()
	_, err := EncodeCursor(b, map[string]string{"history": strings.Repeat("x", 512)})
	require.Error(err)
	s, err := EncodeCursor(b, map[string]string{"days": "2026-10-22|meeting|1|evt:12", "history": "2026-10-22T13:30:00Z|message|msg:31", "upcoming": "2026-10-23T13:30:00Z|meeting|evt:13"})
	require.NoError(err)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(err)
	assert.LessOrEqual(len(raw), 512)
	for _, raw := range []string{`{}`, `{"v":2}`, `{"v":1,"a":"bad","f":"bad","n":1,"s":{}}`, string(raw) + ` {}`, `{"v":1,"extra":true}`, strings.Repeat("x", 513)} {
		_, err := DecodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), b)
		require.ErrorIs(err, ErrCursorExpired)
	}
	_, err = DecodeCursor("bad*", b)
	require.ErrorIs(err, ErrCursorExpired)
	var fields map[string]any
	require.NoError(json.Unmarshal(raw, &fields))
	assert.Len(fields, 5)
}
func TestCursorRejectsChangedWindowAndZone(t *testing.T) {
	b := cursorBinding()
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.Window.To = "2026-10-23"
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
	b = cursorBinding()
	b.Timezone = "America/Toronto"
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorRejectsChangedSourceGrant(t *testing.T) {
	b := cursorBinding()
	b.SourceIDs = []int64{1, 2}
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.SourceIDs = []int64{1}
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorDistinguishesEmptySourceScopeAndNoOpenTools(t *testing.T) {
	b := cursorBinding()
	s, err := EncodeCursor(b, nil)
	requirepkg.NoError(t, err)
	b.SourceIDs = []int64{}
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
	b = cursorBinding()
	b.Request.Scope.OpenTools = []string{}
	_, err = DecodeCursor(s, b)
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
}
func TestCursorPinsRelativeWindowAcrossMidnight(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	b := cursorBinding()
	b.Now = time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)
	b.WindowAt = func(now time.Time) (Window, error) {
		return Window{From: now.AddDate(0, 0, -1).Format(time.DateOnly), To: now.Format(time.DateOnly)}, nil
	}
	s, err := EncodeCursor(b, map[string]string{"history": "key"})
	require.NoError(err)
	b.Now = b.Now.Add(time.Minute * 2)
	b.Window = Window{From: "2026-10-22", To: "2026-10-23"}
	c, err := DecodeCursor(s, b)
	require.NoError(err)
	assert.Equal("2026-10-21", c.Window.From)
	assert.Equal("2026-10-22", c.Window.To)
	assert.Equal(22, c.Now.Day())
}
