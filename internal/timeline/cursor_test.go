package timeline

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cursorBinding() CursorBinding {
	return CursorBinding{ArchiveUID: "synthetic-archive", Request: Request{Mode: ModeDay, Format: "json", Before: 5, After: 5, Scope: Scope{Owner: true}}, Window: Window{From: "2026-10-21", To: "2026-10-22"}, Timezone: "UTC", Now: time.Date(2026, 10, 22, 13, 30, 0, 0, time.UTC)}
}
func TestCursorRoundTrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := cursorBinding()
	encoded, err := EncodeCursor(b, map[string]string{"history": "2026-10-22T13:30:00Z|message|msg:31"})
	requirements.NoError(err)
	b.Now = b.Now.Add(time.Hour)
	b.Request.Budget = 4096
	b.Request.Cursor = "ignored"
	c, err := DecodeCursor(encoded, b)
	requirements.NoError(err)
	assertions.Equal(int64(1792675800), c.Now.Unix())
	assertions.Equal("2026-10-22T13:30:00Z|message|msg:31", c.Positions["history"])
	// Canonicalize unordered set-valued filters without changing the request.
	b.Request.Tiers = []int{2, 1, 2}
	b.Request.Scope.OpenTools = []string{"b", "a"}
	encoded, err = EncodeCursor(b, nil)
	requirements.NoError(err)
	b.Request.Tiers = []int{1, 2}
	b.Request.Scope.OpenTools = []string{"a", "b"}
	_, err = DecodeCursor(encoded, b)
	requirements.NoError(err)
}
func TestCursorPinsFractionalNow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		now     time.Time
		advance time.Duration
	}{
		{"fractional", cursorBinding().Now.Add(123456789 * time.Nanosecond), time.Hour},
		{"midnight", time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC), 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := cursorBinding()
			b.Now = tc.now
			var windowNow time.Time
			b.WindowAt = func(now time.Time) (Window, error) {
				windowNow = now
				return Window{From: now.AddDate(0, 0, -1).Format(time.DateOnly), To: now.Format(time.DateOnly)}, nil
			}
			encoded, err := EncodeCursor(b, map[string]string{"history": "key"})
			require := require.New(t)
			require.NoError(err)
			b.Now = b.Now.Add(tc.advance)
			b.Window = Window{From: b.Now.AddDate(0, 0, -1).Format(time.DateOnly), To: b.Now.Format(time.DateOnly)}
			got, err := DecodeCursor(encoded, b)
			require.NoError(err)
			assert := assert.New(t)
			assert.Equal(tc.now, got.Now)
			assert.Equal(tc.now, windowNow)
			assert.Equal(Window{From: "2026-10-21", To: "2026-10-22"}, got.Window)
		})
	}
}
func cursorFields(t *testing.T) map[string]any {
	t.Helper()
	requirements := require.New(t)
	encoded, err := EncodeCursor(cursorBinding(), map[string]string{"history": "key"})
	requirements.NoError(err)
	_, err = DecodeCursor(encoded, cursorBinding())
	requirements.NoError(err)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal(raw, &fields))
	return fields
}

func TestCursorRejectsInvalidNanoseconds(t *testing.T) {
	requirements := require.New(t)
	b := cursorBinding()
	fields := cursorFields(t)
	for _, nanos := range []int{-1, 1000000000} {
		fields["ns"] = nanos
		raw, err := json.Marshal(fields)
		requirements.NoError(err)
		_, err = DecodeCursor(base64.RawURLEncoding.EncodeToString(raw), b)
		requirements.ErrorIs(err, ErrCursorExpired)
	}
}
func TestCursorRejectsChangedScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []int64
		change  func(*CursorBinding)
	}{
		{"scope", nil, func(b *CursorBinding) {
			b.Request.Scope = Scope{OpenTools: []string{"get_message"}}
		}},
		{"format", nil, func(b *CursorBinding) { b.Request.Format = "markdown" }},
		{"archive", nil, func(b *CursorBinding) { b.ArchiveUID = "other-archive" }},
		{"window", nil, func(b *CursorBinding) { b.Window.To = "2026-10-23" }},
		{"timezone", nil, func(b *CursorBinding) { b.Timezone = "America/Toronto" }},
		{"source grant", []int64{1, 2}, func(b *CursorBinding) { b.SourceIDs = []int64{1} }},
		{"empty source scope", nil, func(b *CursorBinding) { b.SourceIDs = []int64{} }},
		{"no open tools", nil, func(b *CursorBinding) { b.Request.Scope.OpenTools = []string{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := cursorBinding()
			b.SourceIDs = tc.sources
			encoded, err := EncodeCursor(b, nil)
			require.NoError(t, err)
			tc.change(&b)
			_, err = DecodeCursor(encoded, b)
			require.ErrorIs(t, err, ErrCursorExpired)
		})
	}
}
func TestCursorSizeLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := cursorBinding()
	_, err := EncodeCursor(b, map[string]string{"history": strings.Repeat("x", 512)})
	requirements.Error(err)
	s, err := EncodeCursor(b, map[string]string{"days": "2026-10-22|meeting|1|evt:12", "history": "2026-10-22T13:30:00Z|message|msg:31", "upcoming": "2026-10-23T13:30:00Z|meeting|evt:13"})
	requirements.NoError(err)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	requirements.NoError(err)
	assertions.LessOrEqual(len(raw), 512)
	for _, raw := range []string{`{}`, `{"v":2}`, `{"v":1,"a":"bad","f":"bad","n":1,"s":{}}`, string(raw) + ` {}`, `{"v":1,"extra":true}`, strings.Repeat("x", 513)} {
		_, err := DecodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), b)
		requirements.ErrorIs(err, ErrCursorExpired)
	}
	_, err = DecodeCursor("bad*", b)
	requirements.ErrorIs(err, ErrCursorExpired)
}

func TestCursorBindsEffectiveZeroNeighbours(t *testing.T) {
	requirements := require.New(t)
	b := cursorBinding()
	b.Request = anchorRequest("msg:12")
	normalized, err := normalizeRequest(b.Request)
	requirements.NoError(err)
	b.Request = normalized
	omitted, err := EncodeCursor(b, map[string]string{"history": "key"})
	requirements.NoError(err)
	b.Request.Before, b.Request.After = 5, 5
	b.Request.BeforeSet, b.Request.AfterSet = true, true
	_, err = DecodeCursor(omitted, b)
	requirements.NoError(err, "explicit defaults and omitted defaults have identical effective counts")
	for _, before := range []bool{false, true} {
		b.Request = anchorRequest("msg:12")
		if before {
			b.Request.BeforeSet = true
		} else {
			b.Request.AfterSet = true
		}
		b.Request, err = normalizeRequest(b.Request)
		requirements.NoError(err)
		_, err = DecodeCursor(omitted, b)
		requirements.ErrorIs(err, ErrCursorExpired)
		zero, err := EncodeCursor(b, map[string]string{"history": "key"})
		requirements.NoError(err)
		_, err = DecodeCursor(zero, b)
		requirements.NoError(err)
		normalized, err := normalizeRequest(b.Request)
		requirements.NoError(err)
		b.Request = normalized
		_, err = DecodeCursor(zero, b)
		requirements.NoError(err, "Build's normalized request retains the same zero binding")
		b.Request = anchorRequest("msg:12")
		b.Request, err = normalizeRequest(b.Request)
		requirements.NoError(err)
		_, err = DecodeCursor(zero, b)
		requirements.ErrorIs(err, ErrCursorExpired)
	}
}

func TestTimelineCursorRejectsOutOfRangeTime(t *testing.T) {
	requirements := require.New(t)
	b := cursorBinding()
	fields := cursorFields(t)
	for _, seconds := range []int64{253402300800, 9223372036854775807} {
		fields["n"] = seconds
		raw, err := json.Marshal(fields)
		requirements.NoError(err)
		_, err = DecodeCursor(base64.RawURLEncoding.EncodeToString(raw), b)
		requirements.ErrorIs(err, ErrCursorExpired)
	}
}
