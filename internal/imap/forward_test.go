package imap

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestForward(t *testing.T, note string) ReplyDraft {
	t.Helper()
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		Subject: "Original subject", Body: note, QuotedHeader: "From: Parent <parent@example.test>\r\nSubject: Original subject",
		QuotedText: "distinctive quoted text",
		Attachments: []ForwardAttachment{
			{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.test", IsInline: true, Content: []byte("png")},
			{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("pdf")},
		},
	}, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), "forward@example.test")
	require.NoError(t, err)
	return draft
}

func TestBuildForwardPreservesMIME(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	draft := buildTestForward(t, "distinctive current note")
	assertions.Contains(string(draft.Raw), "X-Msgvault-Forward: 1")
	assertions.Equal("Fwd: Original subject", draft.Parsed.Subject)
	assertions.Equal(1, strings.Count(draft.Parsed.BodyText, "distinctive current note"))
	assertions.Contains(draft.Parsed.BodyText, "distinctive quoted text")
	requirements.Len(draft.Parsed.Attachments, 2)
	parts := map[string][2]string{}
	for _, attachment := range draft.Parsed.Attachments {
		parts[attachment.Filename] = [2]string{attachment.ContentID, string(attachment.Content)}
	}
	assertions.Equal(map[string][2]string{"logo.png": {"logo@example.test", "png"}, "report.pdf": {"", "pdf"}}, parts)
}

func TestBuildForwardWrapsAttachmentBase64(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := bytes.Repeat([]byte{'a'}, 201)
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		Attachments: []ForwardAttachment{{Filename: "regular.bin", ContentType: "application/octet-stream", Content: content}},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	encoded := base64.StdEncoding.EncodeToString(content)
	assertions.NotContains(string(draft.Raw), encoded)
	assertions.Contains(string(draft.Raw), encoded[:76]+"\r\n"+encoded[76:152])
	requirements.Len(draft.Parsed.Attachments, 1)
	assertions.Equal(content, draft.Parsed.Attachments[0].Content)
}

func TestBuildForwardReplacementPreservesParts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	draft := buildTestForward(t, "distinctive initial note")
	current := draft
	for _, note := range []string{"distinctive current note", "distinctive final note"} {
		replacement, err := BuildIMAPDraftReplacement(current.Raw, note, time.Now(), "")
		requirements.NoError(err)
		assertions.Equal(1, strings.Count(replacement.Parsed.BodyText, note))
		assertions.NotContains(replacement.Parsed.BodyText, "distinctive initial note")
		assertions.Contains(replacement.Parsed.BodyText, "distinctive quoted text")
		assertions.Contains(string(replacement.Raw), "X-Msgvault-Forward: 1")
		assertions.Equal(draft.Parsed.Attachments, replacement.Parsed.Attachments)
		current = replacement
	}
}

func TestBuildForwardRejectsUnsupportedParts(t *testing.T) {
	_, err := BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"), "changed", time.Now(), "")
	require.ErrorContains(t, err, "does not support")
	_, err = BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nX-Msgvault-Forward: 1\r\nContent-Type: multipart/signed; boundary=x\r\n\r\n--x--\r\n"), "changed", time.Now(), "")
	require.ErrorContains(t, err, "does not support")
	_, err = BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nX-Msgvault-Forward: 1\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n--x--\r\n"), "changed", time.Now(), "")
	require.ErrorContains(t, err, "editable note")
}

func TestBuildIMAPDraftReplacementWithoutContentType(t *testing.T) {
	raw := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: Draft\r\n\r\nOriginal body\r\n")
	draft, err := BuildIMAPDraftReplacement(raw, "Updated body", time.Now(), "updated@example.test")
	require.NoError(t, err)
	assert.Contains(t, draft.Parsed.BodyText, "Updated body")
}
