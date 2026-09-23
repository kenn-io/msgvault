package imap

import (
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLegacyMessageIDFallbackRejectsMalformedBracketsAndBodyText(t *testing.T) {
	for _, raw := range []string{
		"Message-ID: <broken@example.test\r\n\r\nbody",
		"Message-ID: <<nested@example.test>>\r\n\r\nbody",
		"Subject: no identifier\r\n\r\nMessage-ID: 123456789\r\n",
	} {
		assert.Empty(t, rawMIMEMessageID([]byte(raw)))
	}
}
