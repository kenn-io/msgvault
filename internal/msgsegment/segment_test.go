package msgsegment

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/messagebody"
)

func TestSplitReadableCases(t *testing.T) {
	for _, tc := range messagebody.ReadableCases {
		t.Run(tc.Name, func(t *testing.T) {
			var authored strings.Builder
			for _, segment := range Split(tc.Body).Segments {
				if segment.Kind == Authored {
					authored.WriteString(tc.Body[segment.Start:segment.End])
				}
			}
			assert.Equal(t, tc.Want, strings.TrimSpace(authored.String()))
		})
	}
}

func TestSplitRanges(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []Segment
	}{
		{"empty", "", nil},
		{"depth changes", "> a\n> > b\n> c", []Segment{{Quoted, 0, 4, 1}, {Quoted, 4, 10, 2}, {Quoted, 10, 13, 1}}},
		{"blank between depths", "> a\n\n>> b", []Segment{{Quoted, 0, 5, 1}, {Quoted, 5, 9, 2}}},
		{"leading blanks", "\n \t\n> a", []Segment{{Authored, 0, 4, 0}, {Quoted, 4, 7, 1}}},
		{"blank run before authored", "> a\n\n \t\nNew", []Segment{{Quoted, 0, 4, 1}, {Authored, 4, 11, 0}}},
		{"blank run after authored", "New\n\n \t\n> a", []Segment{{Authored, 0, 8, 0}, {Quoted, 8, 11, 1}}},
		{"trailing blank run", "> a\n\n \t", []Segment{{Quoted, 0, 7, 1}}},
		{"all blanks", "\n \t", []Segment{{Authored, 0, 3, 0}}},
		{"separator suffix is prose", "-- Forwarded message -- extra\nOld", []Segment{{Authored, 0, 33, 0}}},
		{"separator needs closing dashes", "-- Original Message\nOld", []Segment{{Authored, 0, 23, 0}}},
		{"signature then original", "-- \nSender\n-- Original Message --\nOld", []Segment{{Signature, 0, 11, 0}, {Quoted, 11, 37, 1}}},
		{"inline and CRLF", "Hié\r\n > old\r\nNew", []Segment{{Authored, 0, 6, 0}, {Quoted, 6, 14, 1}, {Authored, 14, 17, 0}}},
		{"Gmail forward", "New\n---------- Forwarded message ----------\r\nFrom: sender@example.com\r\n\r\nOld", []Segment{{Authored, 0, 4, 0}, {Forwarded, 4, 76, 0}}},
		{"Thunderbird forward", "New\n-------- Forwarded Message --------\nOld", []Segment{{Authored, 0, 4, 0}, {Forwarded, 4, 43, 0}}},
		// https://github.com/thunderbird/thunderbird-l10n/blob/main/de/mail/chrome/messenger/messengercompose/composeMsgs.properties#L181
		{"Thunderbird German original", "New\n-------- Original-Nachricht --------\nOld", []Segment{{Authored, 0, 4, 0}, {Quoted, 4, 44, 1}}},
		// https://github.com/thunderbird/thunderbird-l10n/blob/main/de/mail/chrome/messenger/messengercompose/composeMsgs.properties#L184
		{"Thunderbird German forward CRLF", "New\r\n-------- Weitergeleitete Nachricht --------\r\nOld", []Segment{{Authored, 0, 5, 0}, {Forwarded, 5, 53, 0}}},
		// https://github.com/thunderbird/thunderbird-l10n/blob/main/fr/mail/chrome/messenger/messengercompose/composeMsgs.properties#L181
		{"Thunderbird French original", "New\n-------- Message original --------\nOld", []Segment{{Authored, 0, 4, 0}, {Quoted, 4, 42, 1}}},
		// https://github.com/thunderbird/thunderbird-l10n/blob/main/fr/mail/chrome/messenger/messengercompose/composeMsgs.properties#L184
		{"Thunderbird French forward", "New\n-------- Message transféré --------\nOld", []Segment{{Authored, 0, 4, 0}, {Forwarded, 4, 45, 0}}},
		{"German forward prose", "Weitergeleitete Nachricht von Max anbei.", []Segment{{Authored, 0, 40, 0}}},
		{"French forward prose", "Voici le message transféré :", []Segment{{Authored, 0, 30, 0}}},
		{"French forward needs closing dashes", "-- Message transféré", []Segment{{Authored, 0, 22, 0}}},
		{"French original suffix is prose", "-------- Message original -------- suite", []Segment{{Authored, 0, 40, 0}}},
		// https://github.com/thunderbird/thunderbird-l10n/blob/main/de/mail/chrome/messenger/messengercompose/composeMsgs.properties#L184
		{"signature then German forward", "-- \nSender\n-------- Weitergeleitete Nachricht --------\nOld", []Segment{{Signature, 0, 11, 0}, {Forwarded, 11, 58, 0}}},
		{"begin forward", "Begin forwarded message:\nOld\n> older", []Segment{{Forwarded, 0, 36, 0}}},
		{"signature then forward", "-- \nSender\nBegin forwarded message:\nOld", []Segment{{Signature, 0, 11, 0}, {Forwarded, 11, 39, 0}}},
		{"signature then quote", "-- \nSender\n> old\nNew", []Segment{{Signature, 0, 11, 0}, {Quoted, 11, 17, 1}, {Authored, 17, 20, 0}}},
		{"Original Message", "New\n-----Original Message-----\nFrom: sender@example.com\nOld", []Segment{{Authored, 0, 4, 0}, {Quoted, 4, 59, 1}}},
		{"Original Message nested depth", "-- Original Message --\nOld\n>> older\nOld\n> old", []Segment{{Quoted, 0, 27, 1}, {Quoted, 27, 36, 2}, {Quoted, 36, 45, 1}}},
		{"Outlook nested depth", "From: sender\nSent: today\nSubject: old\nOld\n>> older\nOld\n> old", []Segment{{Quoted, 0, 42, 1}, {Quoted, 42, 51, 2}, {Quoted, 51, 60, 1}}},
		{"contact form without divider", messagebody.ContactForm, []Segment{{Authored, 0, 80, 0}}},
		{"comma before speaker", "On Monday we shipped,\nSomeone wrote:\n> Old", []Segment{{Authored, 0, 37, 0}, {Quoted, 37, 42, 1}}},
		{"header and blank line", "On Tue, Oct 1, 2024, Sender wrote:\n\n> Old\n", []Segment{{Quoted, 0, 42, 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.body)
			assert.Equal(t, tc.want, got.Segments)
			assert.False(t, got.Truncated)
		})
	}
}

func TestSplitThunderbirdReplyHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header string
	}{
		{"German date first", "Am 05.11.2026 um 10:00 schrieb Max <m@x.de>:"},
		{"German sender first", "Max <m@x.de> schrieb am 05.11.2026 um 10:00:"},
		{"French date first", "Le 5 novembre 2026 à 10:00, Max <m@x.fr> a écrit :"},
		{"French sender first", "Max <m@x.fr> a écrit le 5 novembre 2026 à 10:00 :"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("quoted history", func(t *testing.T) {
				body := tc.header + "\n> old"
				assert.Equal(t, []Segment{{Quoted, 0, len(body), 1}}, Split(body).Segments)
				assert.False(t, IsDatedAttribution(tc.header), "CLI attribution recognition must stay unchanged")
			})
			t.Run("authored sentence", func(t *testing.T) {
				body := tc.header + "\nNew text"
				assert.Equal(t, []Segment{{Authored, 0, len(body), 0}}, Split(body).Segments)
			})
			t.Run("requires time", func(t *testing.T) {
				header := strings.Replace(tc.header, "10:00", "noon", 1) + "\n"
				body := header + "> old"
				assert.Equal(t, []Segment{{Authored, 0, len(header), 0}, {Quoted, len(header), len(body), 1}}, Split(body).Segments)
			})
		})
	}
}

func TestSplitScanLimit(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, marker string
		kind                 Kind
		depth                int
	}{
		{"forward continues", "Begin forwarded message:\n", "", Forwarded, 0},
		{"Original Message continues", "-----Original Message-----\n", "", Quoted, 1},
		{"signature cut", "", "-- \n", Authored, 0},
		{"forward cut", "", "Begin forwarded message:\n", Authored, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.prefix + strings.Repeat("a", MaxBodyBytes-len(tc.prefix)-2) + "\n" + tc.marker + "\n> tail"
			got := Split(body)
			assert.True(t, got.Truncated)
			assert.Equal(t, []Segment{{Kind: tc.kind, Start: 0, End: len(body), Depth: tc.depth}}, got.Segments)
		})
	}
	t.Run("retains running label", func(t *testing.T) {
		body := "> " + strings.Repeat("a", MaxBodyBytes-3) + "\nNew\n"
		got := Split(body)
		assert.True(t, got.Truncated)
		assert.Equal(t, []Segment{{Quoted, 0, len(body), 1}}, got.Segments)
	})
	t.Run("Outlook subject beyond scan", func(t *testing.T) {
		prefix := strings.Repeat("a", MaxBodyBytes-len("\nFrom: sender\nSent: today\n")) + "\nFrom: sender\nSent: today\n"
		got := Split(prefix + "Subject: old\nOld")
		assert.True(t, got.Truncated)
		assert.Equal(t, []Segment{{Authored, 0, len(prefix) + 16, 0}}, got.Segments)
	})
	t.Run("partial attribution has no lookahead", func(t *testing.T) {
		body := strings.Repeat("a", MaxBodyBytes-35) + "\nOn Tue, Oct 1, 2024, Sender wrote:\n> Old"
		got := Split(body)
		assert.True(t, got.Truncated)
		assert.Equal(t, []Segment{{Authored, 0, len(body), 0}}, got.Segments)
	})
}

func FuzzSplitTiles(f *testing.F) {
	for _, body := range []string{"", "New\n> Old\n", "-- \r\nSender", "Begin forwarded message:\nOld", "é\r\n> 中", strings.Repeat("a", MaxBodyBytes+1)} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		got := Split(body)
		end := 0
		for _, segment := range got.Segments {
			assert.Equal(t, end, segment.Start)
			require.Greater(t, segment.End, segment.Start)
			require.LessOrEqual(t, segment.End, len(body))
			assert.Contains(t, []Kind{Authored, Quoted, Forwarded, Signature}, segment.Kind)
			end = segment.End
		}
		assert.Equal(t, len(body), end)
	})
}

func TestSplitOutlook(t *testing.T) {
	for _, tc := range []struct {
		name, history string
		quoted        bool
	}{
		{"Windows", "From: sender@example.com\nSent: today\nTo: reader@example.com\nSubject: Earlier\n\nOld claim", true},
		{"divider and blanks", "________________________________\r\n\r\n \t\r\nFrom: sender@example.com\r\nSent: today\r\nSubject: Earlier\r\nOld claim", true},
		{"Mac", "________________________________\nFrom: sender@example.com\nDate: today\nSubject:\nOld claim", true},
		{"Date without divider", messagebody.ContactForm, false},
		{"missing subject", "From: sender@example.com\nSent: today\nOld claim", false},
		{"blank after From", "From: sender@example.com\n\nSent: today\nSubject: Earlier\nOld claim", false},
		{"prose", "From: the team\nWe shipped today", false},
		{"empty From", "From: \nSent: today\nSubject: Earlier\nOld claim", false},
		{"empty Sent", "From: sender@example.com\nSent: \nSubject: Earlier\nOld claim", false},
		{"blank before subject", "From: sender@example.com\nSent: today\n\nSubject: Earlier\nOld claim", false},
		{"eighth line subject", "From: sender@example.com\nSent: today\nTo: reader@example.com\nCc: a@example.com\nCc: b@example.com\nCc: c@example.com\nCc: d@example.com\nSubject: Earlier\nOld claim", true},
		{"ninth line subject", "From: sender@example.com\nSent: today\nTo: reader@example.com\nCc: a@example.com\nCc: b@example.com\nCc: c@example.com\nCc: d@example.com\nCc: e@example.com\nSubject: Earlier\nOld claim", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"New\n", "-- \n"} {
				body := prefix + tc.history
				kind := Authored
				if prefix == "-- \n" {
					kind = Signature
				}
				want := []Segment{{kind, 0, len(body), 0}}
				if tc.quoted {
					want = []Segment{{kind, 0, 4, 0}, {Quoted, 4, len(body), 1}}
				}
				assert.Equal(t, want, Split(body).Segments)
			}
		})
	}
}
