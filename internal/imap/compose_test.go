package imap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildComposePreservesEnvelopeRoles(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	draft, err := BuildCompose(ComposeOptions{
		From:    "Owner <owner@example.test>",
		To:      []string{"To User <to@example.test>"},
		Cc:      []string{"copy@example.test"},
		Bcc:     []string{"hidden@example.test"},
		Subject: "Résumé",
		Body:    "compose body",
	}, time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC), "compose@example.test")
	requirements.NoError(err)
	requirements.Len(draft.Parsed.From, 1)
	requirements.Len(draft.Parsed.To, 1)
	requirements.Len(draft.Parsed.Cc, 1)
	requirements.Len(draft.Parsed.Bcc, 1)
	assertions.Equal("owner@example.test", draft.Parsed.From[0].Email)
	assertions.Equal("copy@example.test", draft.Parsed.Cc[0].Email)
	assertions.Equal("hidden@example.test", draft.Parsed.Bcc[0].Email)
	assertions.Contains(string(draft.Raw), "Bcc: <hidden@example.test>\r\n")
	assertions.Contains(string(draft.Raw), "Content-Transfer-Encoding: quoted-printable\r\n")
}

func TestBuildComposeRejectsMissingOrMalformedRecipients(t *testing.T) {
	requirements := require.New(t)
	for _, options := range []ComposeOptions{
		{From: "owner@example.test", Body: "body"},
		{From: "owner@example.test", To: []string{"broken address"}},
	} {
		_, err := BuildCompose(options, time.Now(), "compose@example.test")
		requirements.Error(err)
	}
}

func TestBuildReplyAllDeduplicatesAndKeepsCc(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	parent := []byte("From: Sender <sender@example.test>\r\n" +
		"Reply-To: Replies <replies@example.test>\r\n" +
		"To: Owner <owner@example.test>, Bob <bob@example.test>\r\n" +
		"Cc: Bob <bob@example.test>, Carol <carol@example.test>\r\n" +
		"Bcc: Secret <secret@example.test>\r\n" +
		"Subject: Topic\r\nMessage-ID: <parent@example.test>\r\n\r\nbody")
	draft, err := BuildReplyWithOptions(parent, "Owner <owner@example.test>", "reply", ReplyOptions{
		ReplyAll: true, SelfAddresses: []string{"alias@example.test"},
	}, time.Now(), "reply@example.test")
	requirements.NoError(err)
	requirements.Len(draft.Parsed.To, 2)
	requirements.Len(draft.Parsed.Cc, 1)
	assertions.Equal("replies@example.test", draft.Parsed.To[0].Email)
	assertions.Equal("bob@example.test", draft.Parsed.To[1].Email)
	assertions.Equal("carol@example.test", draft.Parsed.Cc[0].Email)
	assertions.NotContains(string(draft.Raw), "secret@example.test")
}

func TestBuildReplyAllKeepsCcOnlyResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	parent := []byte("From: Owner <owner@example.test>\r\n" +
		"To: owner@example.test\r\n" +
		"Cc: teammate@example.test\r\n" +
		"Message-ID: <parent@example.test>\r\n\r\nbody")
	draft, err := BuildReplyWithOptions(parent, "owner@example.test", "reply", ReplyOptions{ReplyAll: true}, time.Now(), "reply@example.test")
	requirements.NoError(err)
	assertions.Empty(draft.Parsed.To)
	requirements.Len(draft.Parsed.Cc, 1)
	assertions.Equal("teammate@example.test", draft.Parsed.Cc[0].Email)
}
