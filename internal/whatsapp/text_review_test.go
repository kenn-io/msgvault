package whatsapp

import (
	"context"
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

func TestTextReviewOrdinarySenderLabels(t *testing.T) {
	for _, label := range []string{"You Smith", "You Lin", "Alex Left"} {
		t.Run(label, func(t *testing.T) {
			rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - "+label+": hello"), "dmy", "UTC", textLimitsDefault)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, label, rows[0].Sender)
			assert.Equal(t, "message", rows[0].Kind)
			assert.Equal(t, "hello", rows[0].Body)
		})
	}
}

func TestTextReviewLongMultilineRecord(t *testing.T) {
	body := "garden\n" + strings.Repeat(strings.Repeat("x", 63)+"\n", 4096) + "last"
	raw := "13/6/26, 10:00 - Mira: " + body
	rows, _, err := parseText(t.Context(), strings.NewReader(raw), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, body, rows[0].Body)
	assert.Equal(t, raw, rows[0].Raw)
	assert.Equal(t, 4098, rows[0].EndLine)
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

func TestTextReviewCanceledProgress(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
		return count == 1
	})
	summary, err := NewImporter(st, nil).ImportText(ctx, textFile(t, "13/6/26, 10:00 - Mira: first\n13/6/26, 10:01 - Ivo: second"), textOptions())
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, canceled())
	assert.Equal(t, 1, summary.Records)
	assert.Equal(t, 1, summary.Added)
	var processed, added int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&processed, &added))
	assert.Equal(t, 1, processed)
	assert.Equal(t, 1, added)
}

func TestTextReviewMalformedContinuationHeader(t *testing.T) {
	for _, header := range []string{"13/6/26, noon - Mira: no", "[13/6/26 10:00] Mira: no"} {
		t.Run(header, func(t *testing.T) {
			_, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - Mira: yes\n"+header), "dmy", "UTC", textLimitsDefault)
			require.ErrorContains(t, err, "line 2: unsupported or malformed TXT header")
		})
	}
}

func TestTextReviewUnrecognizedColonLabel(t *testing.T) {
	label := "Mira cambió la descripción del grupo"
	rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - "+label+": flores"), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, label, rows[0].Sender)
	assert.Equal(t, "message", rows[0].Kind)
	assert.Equal(t, "flores", rows[0].Body)
}
