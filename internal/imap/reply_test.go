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
