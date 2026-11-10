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
			rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - Mira: shopping\n"+line), "dmy", "UTC", textLimitsDefault)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "shopping\n"+line, rows[0].Body)
			assert.Equal(t, 2, rows[0].EndLine)
		})
	}
}

func TestTextReviewLongMultilineRecord(t *testing.T) {
	for _, tc := range []struct{ name, raw, body string }{
		{"multiline", "13/6/26, 10:00 - Mira: garden\nseed\nlast", "garden\nseed\nlast"},
		{"header", "13/6/26, 10:00 - Mira: garden", "garden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := textLimits{Bytes: 100, Records: 10, RecordBytes: len(tc.raw)}
			rows, _, err := parseText(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.body, rows[0].Body)
			assert.Equal(t, tc.raw, rows[0].Raw)
			assert.Equal(t, strings.Count(tc.raw, "\n")+1, rows[0].EndLine)
			limits.RecordBytes--
			_, _, err = parseText(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.ErrorContains(t, err, "record exceeds")
		})
	}
}

func TestTextReviewIOSOmittedMedia(t *testing.T) {
	st := testutil.NewTestStore(t)
	body := "\u200eimage omitted"
	input := "[6/13/2026, 10:00:01 AM] Mira: " + body + "\r\n[6/13/2026, 10:01:02 AM] Ivo: image omitted from the list\r\n[6/13/2026, 10:02:03 AM] Mira: image omitted\r\n[6/13/2026, 10:03:04 AM] Mira: asters \u200eimage omitted\r\n"
	opts := textOptions()
	opts.DateOrder = "mdy"
	summary, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, input), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.MediaOmitted)
	assert.Zero(t, summary.MediaSkipped)
	var stored string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT snippet FROM messages ORDER BY id LIMIT 1").Scan(&stored))
	assert.Equal(t, body, stored)
}
