package whatsapp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseTextForTest(ctx context.Context, reader io.Reader, order, zone string, limits textLimits) ([]textRecord, string, error) {
	loc, err := textLocation(order, zone)
	if err != nil {
		return nil, "", err
	}
	return parseText(ctx, reader, order, loc, limits)
}

func TestTextParserHeaders(t *testing.T) {
	for _, tc := range []struct{ name, text, order, wall, precision string }{
		{"android", "13/06/26, 21:25 - Mira: hello\nsecond\n\nlast\n", "dmy", "2026-06-13 21:25:00", "minute"},
		{"ios", "\ufeff\u200e[6/13/2026, 9:25:15\u202fPM] Mira: hello\r\nsecond\r\n\r\nlast\r\n", "mdy", "2026-06-13 21:25:15", "second"},
		{"ymd", "[2026/06/13, 21:25] Mira: hello\nsecond\n\nlast", "ymd", "2026-06-13 21:25:00", "minute"},
		{"dots", "13.06.2026, 9:25 p.m. - Mira: hello\nsecond\n\nlast", "dmy", "2026-06-13 21:25:00", "minute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseTextForTest(t.Context(), strings.NewReader(tc.text), tc.order, "UTC", textLimitsDefault)
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
		{"You Smith: hello", "You Smith", "hello"},
		{"You: fictional garden note", "You", "fictional garden note"},
		{"Mira cambió la descripción del grupo: flores", "Mira cambió la descripción del grupo", "flores"},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseTextForTest(t.Context(), strings.NewReader("13/6/26, 10:00 - "+tc.payload), "dmy", "UTC", textLimitsDefault)
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
		limits                        textLimits
	}{
		{"colon-separator", "13/6/26, 10:00 - Mira: one\n13/6/26, 10:01: Ivo: two", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"senderless-no-comma", "13/6/26, 10:00 - Mira: yes\n13/6/26 10:01 - Messages are encrypted", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"day-month", "13/6/26, 10:00 - Mira: yes\n13 Jun 2026, 10:01 - Ivo: no", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"unicode-digits", "13/6/26, 10:00 - Mira: yes\n١٣/٦/٢٦، ١٠:٠١\u00a0–\u00a0Ivo: two", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"dot-clock", "13/6/26, 10:00 - Mira: yes\n13/6/26 10.01 - Ivo: two", "dmy", "UTC", "line 2: unsupported or malformed TXT header", textLimitsDefault},
		{"preamble", "notes\n13/6/26, 10:00 - Mira: ok", "dmy", "UTC", "line 1: unsupported or malformed TXT header", textLimitsDefault},
		{"invalid-date", "31/2/26, 10:00 - Mira: no", "dmy", "UTC", "day out of range", textLimitsDefault},
		{"bad-clock", "13/6/26, 27:00 - Mira: no", "dmy", "UTC", "hour out of range", textLimitsDefault},
		{"utf8-continuation", "13/6/26, 10:00 - Mira: asters\n\xff", "dmy", "UTC", "line 2: TXT is not valid UTF-8", textLimitsDefault},
		{"empty", "", "dmy", "UTC", "TXT contains no records", textLimitsDefault},
		{"count-limit", "13/6/26, 10:00 - Mira: one\n13/6/26, 10:01 - Mira: two", "dmy", "UTC", "TXT exceeds 1 records", textLimits{Bytes: 100, Records: 1, RecordBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseTextForTest(t.Context(), strings.NewReader(tc.text), tc.order, tc.zone, tc.limits)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestTextParserEarlierFold(t *testing.T) {
	rows, _, err := parseTextForTest(t.Context(), strings.NewReader("25/10/26, 02:30 - Mira: autumn"), "dmy", "Europe/Berlin", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), rows[0].SentAt)
}

func TestTextParserOriginalByteDigest(t *testing.T) {
	for _, input := range []string{"13/6/26, 10:00 - Mira: asters\nseedlings", "\ufeff13/6/26, 10:00 - Mira: asters\r\nseedlings\r\n"} {
		t.Run(fmt.Sprintf("bytes-%d", len(input)), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			limits := textLimitsDefault
			limits.Bytes = int64(len(input))
			_, digest, err := parseTextForTest(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.NoError(err)
			assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(input))), digest)
			limits.Bytes--
			_, _, err = parseTextForTest(t.Context(), strings.NewReader(input), "dmy", "UTC", limits)
			require.ErrorContains(err, "byte limit")
		})
	}
}

func TestTextParserContinuation(t *testing.T) {
	for _, line := range []string{"2026-06-14 deadline", "2026-06-14 deadline - water", "13/6/26 10:01 deadline", "13 Jun 2026, noon - Ivo: two", "13/6/26 noon - Ivo: no", "١٣/٦/٢٦، noon\u00a0–\u00a0Ivo: two", "Plan A - the cheap one", "[link] see this", "[13 Jun noon]Ivo: two", "1/2 cup", "Meet at 10:01", "The price is £3.50", "١٠:٠١"} {
		t.Run(line, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseTextForTest(t.Context(), strings.NewReader("13/6/26, 10:00 - Mira: shopping\n"+line), "dmy", "UTC", textLimitsDefault)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal("shopping\n"+line, rows[0].Body)
			assert.Equal("13/6/26, 10:00 - Mira: shopping\n"+line, rows[0].Raw)
			assert.Equal(1, rows[0].StartLine)
			assert.Equal(2, rows[0].EndLine)
		})
	}
}

func TestTextParserRecordLimit(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"multiline", "13/6/26, 10:00 - Mira: garden\nseed\nlast"},
		{"header", "13/6/26, 10:00 - Mira: garden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			limits := textLimits{Bytes: 100, Records: 10, RecordBytes: len(tc.raw)}
			_, _, err := parseTextForTest(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.NoError(err)
			limits.RecordBytes--
			_, _, err = parseTextForTest(t.Context(), strings.NewReader(tc.raw), "dmy", "UTC", limits)
			require.ErrorContains(err, "record exceeds")
		})
	}
}
