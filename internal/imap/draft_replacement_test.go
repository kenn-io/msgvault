package imap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDraftReplacement(t *testing.T) {
	requirements := require.New(t)
	now := time.Date(2026, time.September, 15, 20, 0, 0, 0, time.UTC)
	result, err := BuildDraftReplacement([]byte("From: Alice <alice@example.com>\r\nTo: Bob <bob@example.com>\r\nCc: Carol <carol@example.com>\r\nBcc: Secret <secret@example.com>\r\nSubject: Question\r\nMessage-ID: <old@example.com>\r\nIn-Reply-To: <parent@example.com>\r\nReferences: <root@example.com> <parent@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nold\r\n"), "new", now, "new@example.com")
	requirements.NoError(err)
	text := string(result.Raw)
	requirements.Contains(text, "From:")
	requirements.Contains(text, "alice@example.com")
	requirements.Contains(text, "To:")
	requirements.Contains(text, "bob@example.com")
	requirements.Contains(text, "Cc:")
	requirements.Contains(text, "carol@example.com")
	requirements.Contains(text, "Bcc:")
	requirements.Contains(text, "secret@example.com")
	requirements.Contains(text, "Message-ID: <new@example.com>")
	requirements.NotContains(text, "Message-ID: <old@example.com>")
	requirements.Contains(text, "In-Reply-To: <parent@example.com>")
	requirements.Contains(text, "References: <root@example.com> <parent@example.com>")
	requirements.Contains(text, "Date: Tue, 15 Sep 2026 20:00:00 +0000")
	requirements.Contains(text, "new")
	requirements.Equal("<new@example.com>", result.Parsed.MessageID)
}

func TestBuildDraftReplacementPreservesReplyTo(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	raw := []byte("From: owner@example.com\r\nTo: recipient@example.com\r\nReply-To: Replies <replies@example.com>\r\n\r\nold body\r\n")
	replacement, err := BuildDraftReplacement(raw, "new body", time.Now(), "replacement@example.com")
	require.NoError(err)
	require.Len(replacement.Parsed.ReplyTo, 1)
	assert.Equal("Replies", replacement.Parsed.ReplyTo[0].Name)
	assert.Equal("replies@example.com", replacement.Parsed.ReplyTo[0].Email)

	reply, err := BuildReply(replacement.Raw, "recipient@example.com", "response", time.Now(), "reply@example.com")
	require.NoError(err)
	require.Len(reply.Parsed.To, 1)
	assert.Equal("replies@example.com", reply.Parsed.To[0].Email)
}

func TestBuildDraftReplacementRejectsMultipart(t *testing.T) {
	requirements := require.New(t)
	_, err := BuildDraftReplacement([]byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"), "new", time.Now(), "")
	requirements.Error(err)
	requirements.ErrorContains(err, "plain-text")
}

func TestBuildDraftReplacementAcceptsEmptyBody(t *testing.T) {
	requirements := require.New(t)
	result, err := BuildDraftReplacement([]byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\nold\r\n"), "", time.Now(), "empty@example.com")
	requirements.NoError(err)
	requirements.Empty(result.Parsed.BodyText)
}

func TestBuildDraftReplacementPreservesBaselineBytes(t *testing.T) {
	requirements := require.New(t)
	currentRaw := []byte("From: Alice Example <alice@example.test>\r\n" +
		"To: Bob Example <bob@example.test>\r\n" +
		"Cc: Carol Example <carol@example.test>\r\n" +
		"Bcc: Secret Example <secret@example.test>\r\n" +
		"Reply-To: Replies Example <replies@example.test>\r\n" +
		"Subject: =?UTF-8?Q?R=C3=A9sum=C3=A9_=E2=9C=93?=\r\n" +
		"Message-ID: <old@example.test>\r\n" +
		"In-Reply-To: <parent@example.test>\r\n" +
		"References: <root@example.test> <parent@example.test> <ancestor@example.test> <older@example.test> <first@example.test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"old\r\n")
	// These bytes came from origin/main's independent BuildDraftReplacement implementation.
	want := "Date: Wed, 23 Sep 2026 12:34:56 +0000\r\n" +
		"From: \"Alice Example\" <alice@example.test>\r\n" +
		"To: \"Bob Example\" <bob@example.test>\r\n" +
		"Cc: \"Carol Example\" <carol@example.test>\r\n" +
		"Bcc: \"Secret Example\" <secret@example.test>\r\n" +
		"Reply-To: \"Replies Example\" <replies@example.test>\r\n" +
		"Subject: =?UTF-8?q?R=C3=A9sum=C3=A9_=E2=9C=93?=\r\n" +
		"Message-ID: <replacement@example.test>\r\n" +
		"In-Reply-To: <parent@example.test>\r\n" +
		"References: <root@example.test> <parent@example.test> <ancestor@example.test>\r\n" +
		" <older@example.test> <first@example.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n"
	replacement, err := BuildDraftReplacement(
		currentRaw, "", time.Date(2026, time.September, 23, 12, 34, 56, 0, time.UTC), "replacement@example.test",
	)
	requirements.NoError(err)
	requirements.Equal([]byte(want), replacement.Raw)
	requirements.Equal("Résumé ✓", replacement.Parsed.Subject)
	requirements.Empty(replacement.Parsed.BodyText)
}

func TestBuildDraftReplacementPreservesCcOnlyEnvelope(t *testing.T) {
	requirements := require.New(t)
	result, err := BuildDraftReplacement([]byte("From: alice@example.com\r\nCc: carol@example.com\r\nBcc: secret@example.com\r\n\r\nold\r\n"), "new", time.Now(), "cc-only@example.com")
	requirements.NoError(err)
	requirements.Empty(result.Parsed.To)
	requirements.Len(result.Parsed.Cc, 1)
	requirements.Len(result.Parsed.Bcc, 1)
}

func TestBuildDraftReplacementRejectsMalformedOrRichDraft(t *testing.T) {
	requirements := require.New(t)
	for _, raw := range []string{
		"From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: broken;\r\n\r\nold",
		"From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/html\r\n\r\nold",
	} {
		_, err := BuildDraftReplacement([]byte(raw), "new", time.Now(), "")
		requirements.Error(err)
	}
	_, err := BuildDraftReplacement([]byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\nold"), "bad\x00body", time.Now(), "")
	requirements.Error(err)
}
