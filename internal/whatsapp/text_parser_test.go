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
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseText(t.Context(), strings.NewReader(tc.text), tc.order, "UTC", textLimitsDefault)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal("Mira", rows[0].Sender)
			assert.Equal("hello\nsecond\n\nlast", rows[0].Body)
			assert.Equal(tc.wall, rows[0].WallTime)
			assert.Equal(tc.precision, rows[0].Precision)
			assert.Equal(1, rows[0].StartLine)
			assert.Equal(4, rows[0].EndLine)
		})
	}
}

func TestTextParserNotices(t *testing.T) {
	for _, tc := range []struct{ payload, sender, body string }{
		{"You changed the group description: garden", "", "You changed the group description: garden"},
		{"Messages and calls are end-to-end encrypted: notice", "", "Messages and calls are end-to-end encrypted: notice"},
		{"Mira: time: noon", "Mira", "time: noon"},
		{"Pat changed the group name: Garden", "", "Pat changed the group name: Garden"},
		{`Mira changed the subject from "Seed: plan" to "Garden"`, "", `Mira changed the subject from "Seed: plan" to "Garden"`},
		{`Mira changed the subject to "Asters: blue"`, "", `Mira changed the subject to "Asters: blue"`},
		{`Mira created group "Garden: seeds"`, "", `Mira created group "Garden: seeds"`},
		{"Mira: I changed the plan: asters", "Mira", "I changed the plan: asters"},
		{"Ivo: You created a garden", "Ivo", "You created a garden"},
		{"You Smith: hello", "You Smith", "hello"},
		{"You Lin: hello", "You Lin", "hello"},
		{"Alex Left: hello", "Alex Left", "hello"},
		{"Mira cambió la descripción del grupo: flores", "Mira cambió la descripción del grupo", "flores"},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - "+tc.payload), "dmy", "UTC", textLimitsDefault)
			require.NoError(err)
			require.Len(rows, 1)
			kind := "message"
			if tc.sender == "" {
				kind = "notice"
			}
			assert.Equal(tc.sender, rows[0].Sender)
			assert.Equal(kind, rows[0].Kind)
			assert.Equal(tc.body, rows[0].Body)
		})
	}
}

func TestTextParserRejects(t *testing.T) {
	for _, tc := range []struct {
		name, text, order, zone, want string
		limits                      textLimits
	}{
		{"preamble", "notes\n13/6/26, 10:00 - Mira: ok", "dmy", "UTC", "line 1: unsupported or malformed TXT header", textLimitsDefault},
		{"invalid-date", "31/2/26, 10:00 - Mira: no", "dmy", "UTC", "day out of range", textLimitsDefault},
		{"bad-clock", "13/6/26, 27:00 - Mira: no", "dmy", "UTC", "hour out of range", textLimitsDefault},
		{"malformed-header", "13/6/26, noon - Mira: no", "dmy", "UTC", "line 1: unsupported or malformed TXT header", textLimitsDefault},
		{"unsupported-header", "[13/6/26 10:00] Mira: no", "dmy", "UTC", "line 1: unsupported or malformed TXT header", textLimitsDefault},
		{"malformed-continuation-header", "13/6/26, 10:00 - Mira: yes\n13/6/26, noon - Mira: no", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"unsupported-continuation-header", "13/6/26, 10:00 - Mira: yes\n[13/6/26 10:00] Mira: no", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"utf8-continuation", "13/6/26, 10:00 - Mira: asters\n\xff", "dmy", "UTC", "line 2: TXT is not valid UTF-8", textLimitsDefault},
		{"utf8", "13/6/26, 10:00 - Mira: \xff", "dmy", "UTC", "line 1: TXT is not valid UTF-8", textLimitsDefault},
		{"empty", "", "dmy", "UTC", "TXT contains no records", textLimitsDefault},
		{"order", "13/6/26, 10:00 - Mira: no", "", "UTC", "--date-order must be", textLimitsDefault},
		{"zone", "13/6/26, 10:00 - Mira: no", "dmy", "Local", "host-dependent Local zone", textLimitsDefault},
		{"count-limit", "13/6/26, 10:00 - Mira: one\n13/6/26, 10:01 - Mira: two", "dmy", "UTC", "TXT exceeds 1 records", textLimits{Bytes: 100, Records: 1, RecordBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseText(t.Context(), strings.NewReader(tc.text), tc.order, tc.zone, tc.limits)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestTextParserEarlierFold(t *testing.T) {
	rows, _, err := parseText(t.Context(), strings.NewReader("1/11/26, 01:30 - Mira: autumn"), "dmy", "America/Vancouver", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC), rows[0].SentAt)
}

func TestTextParserOriginalByteDigest(t *testing.T) {
	for _, input := range []string{"13/6/26, 10:00 - Mira: asters\nseedlings", "\ufeff13/6/26, 10:00 - Mira: asters\r\nseedlings\r\n"} {
		t.Run(fmt.Sprintf("bytes-%d", len(input)), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			limits := textLimitsDefault
			limits.Bytes = int64(len(input))
			rows, digest, err := parseText(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(input))), digest)
			assert.Equal("asters\nseedlings", rows[0].Body)
			assert.Equal("13/6/26, 10:00 - Mira: asters\nseedlings", rows[0].Raw)
			limits.Bytes--
			_, _, err = parseText(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.ErrorContains(err, "byte limit")
		})
	}
}
