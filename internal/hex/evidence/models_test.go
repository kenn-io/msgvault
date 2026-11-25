package evidence

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveTitleSourceClockFormats(t *testing.T) {
	created := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct{ app, title, source, want string }{
		{"Zoom", "Zoom call — 2:30 PM", "", "default"},
		{"Zoom", "Zoom call — 14:30", "", "default"},
		{"", "Meeting — 14:30", "", "default"},
		{"Zoom", "Zoom call — 14:31", "", "user"},
		{"Zoom", "Zoom call — 14:30", "user", "user"},
	} {
		t.Run(tc.title+tc.source, func(t *testing.T) {
			m := Manifest{App: tc.app, Title: tc.title, TitleSource: tc.source, CreatedAtMS: created}
			m.ResolveTitleSource(time.UTC)
			assert.Equal(t, tc.want, m.TitleSource)
		})
	}
}
