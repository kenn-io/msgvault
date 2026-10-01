package imap

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
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
	assertions.Contains(draft.Parsed.BodyText, "---------- Forwarded message ----------")
	requirements.Len(draft.Parsed.Attachments, 2)
	parts := map[string][2]string{}
	for _, attachment := range draft.Parsed.Attachments {
		parts[attachment.Filename] = [2]string{attachment.ContentID, string(attachment.Content)}
	}
	assertions.Equal(map[string][2]string{"logo.png": {"logo@example.test", "png"}, "report.pdf": {"", "pdf"}}, parts)
}

func TestBuildForwardAttachmentWireFormat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attachment  ForwardAttachment
		encoding    string
		disposition string
	}{
		{
			name: "attached message",
			attachment: ForwardAttachment{Filename: "original.eml", ContentType: "message/rfc822",
				Content: []byte("From: sender@example.test\r\nSubject: Original\r\n\r\nCaf\xc3\xa9\r\n")},
			encoding: "8bit", disposition: "attachment",
		},
		{
			name: "unnamed inline image",
			attachment: ForwardAttachment{ContentType: "image/png", ContentID: "logo@example.test",
				IsInline: true, Content: []byte("image")},
			encoding: "base64", disposition: "inline",
		},
		{
			name: "extension disposition",
			attachment: ForwardAttachment{Filename: "data.bin", ContentType: "application/octet-stream",
				Disposition: "x-custom", Content: []byte("data")},
			encoding: "base64", disposition: "attachment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			draft, err := BuildForward(ForwardOptions{
				From: "sender@example.test", To: []string{"recipient@example.test"},
				Attachments: []ForwardAttachment{tc.attachment},
			}, time.Now(), "forward@example.test")
			requirements.NoError(err)
			message, err := mail.ReadMessage(bytes.NewReader(draft.Raw))
			requirements.NoError(err)
			_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
			requirements.NoError(err)
			reader := multipart.NewReader(message.Body, params["boundary"])
			for range 2 { // editable note and quoted original
				_, err = reader.NextRawPart()
				requirements.NoError(err)
			}
			part, err := reader.NextRawPart()
			requirements.NoError(err)
			assertions.Equal(tc.encoding, part.Header.Get("Content-Transfer-Encoding"))
			disposition, dispositionParams, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			requirements.NoError(err)
			assertions.Equal(tc.disposition, disposition)
			if tc.attachment.Filename == "" {
				_, typeParams, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
				requirements.NoError(err)
				assertions.NotContains(typeParams, "name")
				assertions.NotContains(dispositionParams, "filename")
			}
			if tc.encoding == "8bit" {
				assertions.Equal("8bit", message.Header.Get("Content-Transfer-Encoding"))
				content, err := io.ReadAll(part)
				requirements.NoError(err)
				assertions.Equal(tc.attachment.Content, content)
				replacement, err := BuildIMAPDraftReplacement(draft.Raw, "updated note", time.Now(), "")
				requirements.NoError(err)
				assertions.Equal(draft.Parsed.Attachments, replacement.Parsed.Attachments)
			}
		})
	}
}

func TestBuildForwardRejectsBinaryMessage(t *testing.T) {
	for _, content := range []string{"Subject: Original\r\n\r\nzero\x00byte", "Subject: Original\r\n\r\n" + strings.Repeat("x", 999)} {
		_, err := BuildForward(ForwardOptions{
			From: "sender@example.test", To: []string{"recipient@example.test"},
			Attachments: []ForwardAttachment{{Filename: "original.eml", ContentType: "message/rfc822", Content: []byte(content)}},
		}, time.Now(), "forward@example.test")
		require.ErrorContains(t, err, "binary transport")
	}
}

func TestBuildForwardSubject(t *testing.T) {
	for _, subject := range []string{"Fw: Original", "fWd: FW: Original", "Original"} {
		draft, err := BuildForward(ForwardOptions{
			From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: subject,
		}, time.Now(), "forward@example.test")
		require.NoError(t, err)
		assert.Equal(t, "Fwd: Original", draft.Parsed.Subject)
	}
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
