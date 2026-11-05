package gvoice

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func callHTML(title, phone, published, duration string) string {
	return fmt.Sprintf(`<?xml version="1.0" ?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>%s</title></head>
<body><div class="haudio">
<div class="contributor vcard"><a class="tel" href="tel:%s"><span class="fn">Test User</span></a></div>
<abbr class="published" title="%s">Jan 9, 2024</abbr>
<abbr class="duration" title="%s">(00:00:00)</abbr>
</div></body></html>`, title, phone, published, duration)
}

// Person tiers count answered calls by connected length, so the importer
// records it for received and placed calls only.
func TestImportRecordsConnectedCallLength(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	voice := writeVoice(t)
	calls := filepath.Join(voice, "Calls")
	files := map[string]string{
		"Test User - Placed - 2024-01-09T08_04_05Z.html":   callHTML("Placed call to Test User", "+15550000008", "2024-01-09T03:04:05.000-05:00", "PT1H2M3S"),
		"Test User - Received - 2024-01-10T08_04_05Z.html": callHTML("Received call from Test User", "+15550000008", "2024-01-10T03:04:05.000-05:00", "PT45S"),
		"Test User - Missed - 2024-01-11T08_04_05Z.html":   callHTML("Missed call from Test User", "+15550000008", "2024-01-11T03:04:05.000-05:00", "PT30S"),
	}
	for name, html := range files {
		require.NoError(os.WriteFile(filepath.Join(calls, name), []byte(html), 0o600))
	}
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, "")

	rows, err := st.DB().Query(st.Rebind(`SELECT snippet, metadata FROM messages WHERE message_type = ? ORDER BY sent_at`), store.MessageTypeGoogleVoiceCall)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var snippet string
		var metadata *string
		require.NoError(rows.Scan(&snippet, &metadata))
		got[snippet] = ""
		if metadata != nil {
			got[snippet] = *metadata
		}
	}
	require.NoError(rows.Err())
	require.Len(got, 3)
	assert.JSONEq(`{"call_connected_seconds":3723}`, got["Placed call to Test User (1h 2m 3s)"])
	assert.JSONEq(`{"call_connected_seconds":45}`, got["Received call from Test User (45s)"])
	assert.Empty(got["Missed call from Test User (30s)"], "a missed call never connected")
}

func TestDurationSeconds(t *testing.T) {
	assert := assert.New(t)
	for iso, want := range map[string]int{"PT1M23S": 83, "PT2H": 7200, "PT3.9S": 3, "": 0, "P1D": 0, "PTX": 0} {
		assert.Equal(want, durationSeconds(iso), iso)
	}
}
