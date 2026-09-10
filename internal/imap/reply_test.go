package imap

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	parent := "From: Test User <user@example.com>\r\n" +
		"To: alice@example.com\r\n" +
		"Reply-To: Test User <user@example.com>\r\n" +
		"Subject: Re: Café notes\r\n" +
		"Message-ID: <parent@example.com>\r\n" +
		"References: <root@example.com>\r\n" +
		"\r\nparent body\r\n"
	draft, err := BuildReply([]byte(parent), "alice@example.com", "body-secret-731\nsecond line", time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC), "reply@example.com")
	requirements.NoError(err)
	t.Log(`reply composition accepts --body= and emits full References plus CRLF text/plain MIME`)
	assertions.Contains(string(draft.Raw), "In-Reply-To: <parent@example.com>")
	assertions.Contains(string(draft.Raw), "References: <root@example.com> <parent@example.com>")
	assertions.Contains(string(draft.Raw), "Subject: =?UTF-8?")
	assertions.Contains(string(draft.Raw), "Content-Transfer-Encoding: quoted-printable")
	assertions.Equal("Re: Café notes", draft.Parsed.Subject)
	assertions.Equal("body-secret-731\nsecond line", strings.TrimSpace(strings.ReplaceAll(draft.Parsed.BodyText, "\r\n", "\n")))
}

func TestBuildReplyRejectsInvalidParentHeaders(t *testing.T) {
	for _, parent := range []string{
		"From: user@example.com\r\nFrom: alice@example.com\r\n\r\nbody",
		"From: user@example.com\r\nMessage-ID: broken <id>\r\n\r\nbody",
	} {
		_, err := BuildReply([]byte(parent), "alice@example.com", "body", time.Now(), "reply@example.com")
		require.Error(t, err)
	}
}

func TestBuildReplyDoesNotCopyParentInjectedHeaders(t *testing.T) {
	parent := "From: user@example.com\r\n" +
		"Subject: safe\r\n" +
		"Bcc: attacker@example.com\r\n" +
		"Message-ID: <parent@example.com>\r\n\r\nbody"
	draft, err := BuildReply([]byte(parent), "alice@example.com", "body", time.Now(), "reply@example.com")
	require.NoError(t, err)
	assert.NotContains(t, string(draft.Raw), "Bcc:")
}

func TestBuildReplyDecodesEncodedParentSubject(t *testing.T) {
	parent := "From: user@example.com\r\n" +
		"Subject: =?UTF-8?Q?Caf=C3=A9_notes?=\r\n" +
		"Message-ID: <parent@example.com>\r\n\r\nbody"
	draft, err := BuildReply([]byte(parent), "alice@example.com", "body", time.Now(), "reply@example.com")
	require.NoError(t, err)
	assert.Equal(t, "Re: Café notes", draft.Parsed.Subject)
	assert.Contains(t, string(draft.Raw), "Subject: =?UTF-8?")
}

func TestBuildReplyParsesCommentedThreadingHeaders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	parent := "From: user@example.com\r\n" +
		"Message-ID: <parent@example.com> (sent by test)\r\n" +
		"References: <root@example.com> (nested (comment)) <mid@example.com>\r\n" +
		"\r\nbody"
	draft, err := BuildReply([]byte(parent), "alice@example.com", "body", time.Now(), "reply@example.com")
	requirements.NoError(err)
	raw := string(draft.Raw)
	assertions.Contains(raw, "In-Reply-To: <parent@example.com>\r\n")
	assertions.Contains(raw, "References: <root@example.com> <mid@example.com> <parent@example.com>\r\n")
	assertions.NotContains(raw, "comment")

	parent = "From: user@example.com\r\n" +
		"Message-ID: <parent@example.com>\r\n" +
		"In-Reply-To: <a@example.com> <b@example.com>\r\n" +
		"\r\nbody"
	draft, err = BuildReply([]byte(parent), "alice@example.com", "body", time.Now(), "reply@example.com")
	requirements.NoError(err)
	assertions.Contains(string(draft.Raw), "References: <a@example.com> <b@example.com> <parent@example.com>\r\n")

	for _, bad := range []string{
		"From: user@example.com\r\nMessage-ID: <one@example.com> <two@example.com>\r\n\r\nbody",
		"From: user@example.com\r\nMessage-ID: <parent@example.com> (unterminated\r\n\r\nbody",
		"From: user@example.com\r\nReferences: <root@example.com\r\n\r\nbody",
	} {
		_, err := BuildReply([]byte(bad), "alice@example.com", "body", time.Now(), "reply@example.com")
		assertions.Error(err)
	}
}

func TestReplaceDraftBodyPreservesHeaders(t *testing.T) {
	draftRawBytes := []byte("From: alice@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Re: Question\r\n" +
		"Message-ID: <old-id@example.com>\r\n" +
		"In-Reply-To: <parent@example.com>\r\n" +
		"References: <root@example.com> <parent@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\nOriginal draft body\r\n")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("preserves headers and replaces body", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		result, err := ReplaceDraftBody(draftRawBytes, "new body text", now)
		requirements.NoError(err)
		raw := string(result.Raw)
		assertions.Contains(raw, "From: alice@example.com")
		assertions.Contains(raw, "To: bob@example.com")
		assertions.Contains(raw, "Subject: Re: Question")
		assertions.Contains(raw, "In-Reply-To: <parent@example.com>")
		assertions.Contains(raw, "References:")
		assertions.Contains(raw, "<root@example.com>")
		assertions.Contains(raw, "<parent@example.com>")
		// Message-ID must be new.
		assertions.NotContains(raw, "old-id@example.com")
		assertions.Contains(raw, "Message-ID:")
		// Body must be the new text.
		assertions.Equal("new body text", strings.TrimSpace(strings.ReplaceAll(result.Parsed.BodyText, "\r\n", "\n")))
	})

	t.Run("empty body is valid", func(t *testing.T) {
		requirements := require.New(t)
		result, err := ReplaceDraftBody(draftRawBytes, "", now)
		requirements.NoError(err)
		require.NotNil(t, result.Parsed)
	})

	t.Run("attachment-bearing draft returns invalid_message", func(t *testing.T) {
		multipartRaw := []byte("From: alice@example.com\r\n" +
			"To: bob@example.com\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"boundary\"\r\n" +
			"\r\n--boundary\r\n" +
			"Content-Type: text/plain\r\n\r\nHello\r\n" +
			"--boundary--\r\n")
		_, err := ReplaceDraftBody(multipartRaw, "body", now)
		require.ErrorContains(t, err, "invalid_message")
	})
}

func TestParseMessageIDHeader(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ids, err := parseMessageIDHeader(" <a@example.com>(c)<b@example.com> bare@example.com (x \\) y) ")
	requirements.NoError(err)
	assertions.Equal([]string{"a@example.com", "b@example.com", "bare@example.com"}, ids)
	ids, err = parseMessageIDHeader("")
	requirements.NoError(err)
	assertions.Empty(ids)
	for _, bad := range []string{"<>", "<a b@example.com>", "a) b", "<a@example.com>>", "(open"} {
		_, err := parseMessageIDHeader(bad)
		assertions.Error(err, bad)
	}
}
