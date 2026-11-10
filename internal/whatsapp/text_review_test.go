package whatsapp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextReviewNumericContinuation(t *testing.T) {
	for _, line := range []string{"2-3 days", "1/2 cup", "3.5 stars", "2026-06-14 deadline", "2026-06-14 deadline - water"} {
		t.Run(line, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - Mira: shopping\n"+line), "dmy", "UTC", textLimitsDefault)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal("shopping\n"+line, rows[0].Body)
			assert.Equal(2, rows[0].EndLine)
		})
	}
}

func TestTextReviewLongMultilineRecord(t *testing.T) {
	for _, tc := range []struct{ name, raw, body string }{
		{"multiline", "13/6/26, 10:00 - Mira: garden\nseed\nlast", "garden\nseed\nlast"},
		{"header", "13/6/26, 10:00 - Mira: garden", "garden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			limits := textLimits{Bytes: 100, Records: 10, RecordBytes: len(tc.raw)}
			rows, _, err := parseText(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(tc.body, rows[0].Body)
			assert.Equal(tc.raw, rows[0].Raw)
			assert.Equal(strings.Count(tc.raw, "\n")+1, rows[0].EndLine)
			limits.RecordBytes--
			_, _, err = parseText(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.ErrorContains(err, "record exceeds")
		})
	}
}

func TestTextReviewIOSOmittedMedia(t *testing.T) {
	for _, tc := range []struct {
		body    string
		omitted int
	}{
		{"\u200eimage omitted", 1},
		{"image omitted from the list", 0},
		{"image omitted", 0},
		{"asters \u200eimage omitted", 1},
		{"\u200evideo omitted", 1},
		{"\u200esticker omitted", 1},
		{"\u200eGIF omitted", 1},
		{"\u200edocument omitted", 1},
		{"\u200eContact card omitted", 1},
		{"asters \u200eimage omitted \u200e<This message was edited>", 1},
		{"video omitted", 0},
	} {
		t.Run(tc.body, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			opts := textOptions()
			opts.DateOrder = "mdy"
			input := "[6/13/2026, 10:00:01 AM] Mira: " + tc.body + "\r\n"
			summary, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, input), opts)
			require.NoError(err)
			assert.Equal(tc.omitted, summary.MediaOmitted)
			assert.Zero(summary.MediaSkipped)
			var stored string
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT snippet FROM messages").Scan(&stored))
			assert.Equal(tc.body, stored)
		})
	}
}
