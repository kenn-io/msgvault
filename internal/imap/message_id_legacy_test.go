package imap

import (
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/msgvault/internal/mime"
)

func TestLegacyMessageIDPreservedAcrossIMAPFetchPaths(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"bare historical ID", "123456789", "123456789"},
		{"bracketed historical ID", "<[legacy-token==@example.test]>", "[legacy-token==@example.test]"},
		{"standard ID", "<standard@example.test>", "standard@example.test"},
		{"comment", "<comment@example.test> (comment)", "comment@example.test"},
		{"missing closing bracket", "<1.3.123456.20080806021507@mail.example.test", "1.3.123456.20080806021507@mail.example.test"},
		{"trailing parameters", `<ABCDEF0123456789@mail01.example.test> type="multipart/alternative"`, "ABCDEF0123456789@mail01.example.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			header := []byte("Message-ID: " + tt.header + "\r\n\r\n")
			raw := append(append([]byte{}, header...), []byte("body")...)
			assert.Equal(tt.want, rawMIMEMessageID(header))
			assert.Equal(tt.want, rawMIMEMessageID(raw))

			client := Client{selectedUIDValidity: 1}
			message := fetchMessageBufferWithoutEnvelope(header)
			var unidentified []imapapi.UID
			identities := make(map[string]bool)
			client.recordMessageIDResults("INBOX", identities, &unidentified, []*imapclient.FetchMessageBuffer{message})
			assert.Empty(unidentified)
			assert.True(identities[tt.want])
			require.Len(client.observedMemberships, 1)
			assert.Equal(tt.want, client.observedMemberships[0].RFC822MessageID)

			labels := newLabelBatchResults([]string{"INBOX|10"})
			client.applyLabelFetchResults(labels, map[imapapi.UID]int{10: 0}, "INBOX", nil, []*imapclient.FetchMessageBuffer{message})
			require.NoError(labels[0].Err)
			assert.Equal(tt.want, labels[0].RFC822MessageID)
		})
	}
}

func TestLegacyMessageIDFallbackRejectsNestedBracketsAndBodyText(t *testing.T) {
	for _, raw := range []string{
		"Message-ID: <<nested@example.test>>\r\n\r\nbody",
		"Subject: no identifier\r\n\r\nMessage-ID: 123456789\r\n",
	} {
		assert.Empty(t, rawMIMEMessageID([]byte(raw)))
	}
}

func TestLegacyMessageIDSourceValidation(t *testing.T) {
	for _, test := range []struct {
		name, stored, actual      string
		wantMatch, wantConclusive bool
	}{
		{"missing closing bracket", "<legacy@example.test", "legacy@example.test", false, false},
		{"trailing parameters", `<Local@EXAMPLE.TEST> type="multipart/alternative"`, "Local@example.test", false, false},
		{"local case differs", "<Local@example.test", "local@example.test", false, true},
		{"malformed values differ", "<<one@example.test>>", "<<two@example.test>>", false, true},
		{"nested versus clean", "<<legacy@example.test>>", "legacy@example.test", false, true},
		{"space inside brackets", "< legacy@example.test>", "legacy@example.test", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := Client{}
			matches, conclusive, err := client.FetchedSourceMessageMatches("INBOX|1", test.stored, test.actual)
			require.NoError(t, err)
			assert.Equal(t, test.wantConclusive, conclusive)
			assert.Equal(t, test.wantMatch, matches)
		})
	}
	for _, test := range []struct {
		name                      string
		observedUIDValidity       uint32
		wantMatch, wantConclusive bool
	}{
		{"unchanged epoch proves legacy identity", 1, true, true},
		{"changed epoch invalidates legacy identity", 2, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := Client{
				priorFolderStates:    map[string]FolderState{"INBOX": {UIDValidity: 1}},
				observedFolderStates: map[string]FolderState{"INBOX": {UIDValidity: test.observedUIDValidity}},
			}
			matches, conclusive, err := client.FetchedSourceMessageMatches("INBOX|1", "<legacy@example.test", "legacy@example.test")
			require.NoError(t, err)
			assert.Equal(t, test.wantConclusive, conclusive)
			assert.Equal(t, test.wantMatch, matches)
		})
	}
}

func TestHegelMessageIDContract(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		atom := func(alphabet string) string {
			n := hegel.Draw(ht, hegel.Integers(1, 8))
			value := make([]byte, n)
			for i := range value {
				value[i] = alphabet[hegel.Draw(ht, hegel.Integers(0, len(alphabet)-1))]
			}
			return string(value)
		}
		local := atom("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_+-")
		if hegel.Draw(ht, hegel.Booleans()) {
			local += "." + atom("abcdefghijklmnopqrstuvwxyz0123456789_+-")
		}
		id := local + "@" + atom("abcdefghijklmnopqrstuvwxyz0123456789") + ".example.test"
		legacy := "[" + local + "==@example.test]"
		digits := atom("0123456789")
		form := hegel.Draw(ht, hegel.Integers(0, 9))
		var header, wantIMAP, wantStorage string
		switch form {
		case 0:
			header, wantIMAP, wantStorage = "<"+id+">", id, id
		case 1:
			header, wantIMAP, wantStorage = id, id, id
		case 2:
			header, wantIMAP, wantStorage = digits, digits, digits
		case 3:
			header, wantIMAP, wantStorage = "<"+legacy+">", legacy, legacy
		case 4:
			header = "<<" + id + ">>"
		case 5:
			header = id + ">"
		case 6:
			header = "<>"
		case 7:
			header, wantIMAP = "<"+id, id
		case 8:
			header, wantIMAP = "<"+id+"> type=\"text/plain\"", id
		case 9:
			header, wantIMAP = "<"+id+"> (comment)", id
		}
		fold := hegel.Draw(ht, hegel.SampledFrom([]string{" ", "\r\n\t"}))
		raw := []byte("Message-ID:" + fold + header + "\r\n\r\nMessage-ID: decoy@example.test\r\n")
		if hegel.Draw(ht, hegel.Booleans()) {
			raw = []byte("Subject: no identifier\r\n\r\nMessage-ID: <" + id + ">\r\n")
			wantIMAP, wantStorage = "", ""
		}
		ht.Logf("form=%d raw=%q", form, raw)
		parsed, err := mime.Parse(raw)
		require.NoError(ht, err)
		assert.Equal(ht, wantIMAP, rawMIMEMessageID(raw))
		assert.Equal(ht, wantStorage, mime.NormalizeMessageID(parsed.MessageID))
	})
}
