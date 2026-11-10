package whatsapp

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextParserHeaders(t *testing.T) {
	for _, tc := range []struct{ name, text, order, wall, precision string }{
		{"android", "13/06/26, 21:25 - Mira: hello\nsecond\n\nlast\n", "dmy", "2026-06-13 21:25:00", "minute"},
		{"ios", "\ufeff\u200e[6/13/2026, 9:25:15\u202fPM] Mira: hello\r\nsecond\r\n\r\nlast\r\n", "mdy", "2026-06-13 21:25:15", "second"},
		{"ymd", "[2026/06/13, 21:25] Mira: hello\nsecond\n\nlast", "ymd", "2026-06-13 21:25:00", "minute"},
		{"dots", "13.06.2026, 9:25 p.m. - Mira: hello\nsecond\n\nlast", "dmy", "2026-06-13 21:25:00", "minute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, digest, err := parseText(t.Context(), strings.NewReader(tc.text), tc.order, "UTC", textLimitsDefault)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Len(t, digest, 64)
			assert.Equal(t, "Mira", rows[0].Sender)
			assert.Equal(t, "hello\nsecond\n\nlast", rows[0].Body)
			assert.Equal(t, tc.wall, rows[0].WallTime)
			assert.Equal(t, tc.precision, rows[0].Precision)
			assert.Equal(t, 1, rows[0].StartLine)
			assert.Equal(t, 4, rows[0].EndLine)
		})
	}
}

func TestTextParserNotices(t *testing.T) {
	rows, _, err := parseText(t.Context(), strings.NewReader("13/06/2026, 10:00 - You changed the group description: garden\n13/06/2026, 10:01 - Messages and calls are end-to-end encrypted: notice\n13/06/2026, 10:02 - Mira: time: noon\n13/06/2026, 10:03 - Pat changed the group name: Garden\n13/06/2026, 10:04 - Mira changed the subject from \"Seed: plan\" to \"Garden\"\n13/06/2026, 10:05 - Mira changed the subject to \"Asters: blue\"\n13/06/2026, 10:06 - Mira created group \"Garden: seeds\"\n"), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 7)
	for _, i := range []int{0, 1, 3, 4, 5, 6} {
		assert.Empty(t, rows[i].Sender)
		assert.Equal(t, "notice", rows[i].Kind)
	}
	assert.Equal(t, "time: noon", rows[2].Body)
	assert.Equal(t, `Mira changed the subject from "Seed: plan" to "Garden"`, rows[4].Body)
	assert.Equal(t, `Mira changed the subject to "Asters: blue"`, rows[5].Body)
	assert.Equal(t, `Mira created group "Garden: seeds"`, rows[6].Body)
}

func TestTextParserRejects(t *testing.T) {
	for _, tc := range []struct {
		name, text, order, zone string
		limits                  textLimits
	}{
		{"preamble", "notes\n13/6/26, 10:00 - Mira: ok", "dmy", "UTC", textLimitsDefault},
		{"invalid-date", "31/2/26, 10:00 - Mira: no", "dmy", "UTC", textLimitsDefault},
		{"bad-clock", "13/6/26, 27:00 - Mira: no", "dmy", "UTC", textLimitsDefault},
		{"malformed-header", "13/6/26, noon - Mira: no", "dmy", "UTC", textLimitsDefault},
		{"unsupported-header", "[13/6/26 10:00] Mira: no", "dmy", "UTC", textLimitsDefault},
		{"utf8-continuation", "13/6/26, 10:00 - Mira: asters\n\xff", "dmy", "UTC", textLimitsDefault},
		{"utf8", "13/6/26, 10:00 - Mira: \xff", "dmy", "UTC", textLimitsDefault},
		{"empty", "", "dmy", "UTC", textLimitsDefault},
		{"order", "13/6/26, 10:00 - Mira: no", "", "UTC", textLimitsDefault},
		{"zone", "13/6/26, 10:00 - Mira: no", "dmy", "Local", textLimitsDefault},
		{"gap", "8/3/26, 02:30 - Mira: no", "dmy", "America/Vancouver", textLimitsDefault},
		{"file-limit", "13/6/26, 10:00 - Mira: no", "dmy", "UTC", textLimits{Bytes: 10, Records: 10, RecordBytes: 100}},
		{"record-limit", "13/6/26, 10:00 - Mira: no", "dmy", "UTC", textLimits{Bytes: 100, Records: 10, RecordBytes: 10}},
		{"count-limit", "13/6/26, 10:00 - Mira: one\n13/6/26, 10:01 - Mira: two", "dmy", "UTC", textLimits{Bytes: 100, Records: 1, RecordBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseText(t.Context(), strings.NewReader(tc.text), tc.order, tc.zone, tc.limits)
			require.Error(t, err)
		})
	}
}

func TestTextParserEarlierFold(t *testing.T) {
	rows, _, err := parseText(t.Context(), strings.NewReader("1/11/26, 01:30 - Mira: autumn"), "dmy", "America/Vancouver", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC), rows[0].SentAt)
}

func TestTextParserNoticeWordsInMessage(t *testing.T) {
	rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - Mira: I changed the plan: asters\n13/6/26, 10:01 - Ivo: You created a garden\n"), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "Mira", rows[0].Sender)
	assert.Equal(t, "I changed the plan: asters", rows[0].Body)
	assert.Equal(t, "Ivo", rows[1].Sender)
}

func TestTextParserOriginalByteDigest(t *testing.T) {
	for _, input := range []string{"13/6/26, 10:00 - Mira: asters\nseedlings", "\ufeff13/6/26, 10:00 - Mira: asters\r\nseedlings\r\n"} {
		t.Run(fmt.Sprintf("bytes-%d", len(input)), func(t *testing.T) {
			limits := textLimitsDefault
			limits.Bytes = int64(len(input))
			rows, digest, err := parseText(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(input))), digest)
			assert.Equal(t, "asters\nseedlings", rows[0].Body)
			assert.Equal(t, "13/6/26, 10:00 - Mira: asters\nseedlings", rows[0].Raw)
			limits.Bytes--
			_, _, err = parseText(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.ErrorContains(t, err, "byte limit")
		})
	}
}
