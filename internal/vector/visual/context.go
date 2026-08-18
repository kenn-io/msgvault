package visual

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

type MessageContext struct {
	Subject       string
	Body          string
	MessageType   string
	Filename      string
	ThreadHistory string
	Labels        []string
}

type ContextPolicy struct {
	MaxChars           int
	InputVersion       string
	EligibilityVersion string
}

type AssembleRequest struct {
	Owner          Owner
	Media          *MediaInput
	Context        MessageContext
	Role           store.AttachmentRole
	SourceSequence int64
	Policy         ContextPolicy
}

func AssembleDocument(request AssembleRequest) (DocumentInput, bool, error) {
	if request.Media == nil {
		return DocumentInput{}, false, errors.New("visual document media is required")
	}
	if request.Owner.MediaInputKey == "" {
		request.Owner.MediaInputKey = OriginalMediaInputKey
	}
	if request.Owner.BlobHash == "" || request.Media.BlobHash == "" || request.Owner.BlobHash != request.Media.BlobHash {
		return DocumentInput{}, false, errors.New("visual document owner and media hashes must match")
	}
	if request.Policy.MaxChars <= 0 {
		request.Policy.MaxChars = 4000
	}
	contextText, truncated := normalizedMessageContext(request.Context, request.Policy.MaxChars)
	parts := make([]InputPart, 0, 2)
	if contextText != "" {
		parts = append(parts, InputPart{Text: contextText})
	}
	parts = append(parts, InputPart{Media: request.Media})
	revision := publishedRevision(request, contextText)
	return DocumentInput{
		Owner: request.Owner, Revision: revision,
		SourceSequence: request.SourceSequence, Parts: parts,
	}, truncated, nil
}

func normalizedMessageContext(context MessageContext, maxChars int) (string, bool) {
	text, preprocessingTruncated := preprocess.Preprocess(context.Subject, context.Body, 0, preprocess.Config{
		StripQuotes: true, StripSignatures: true, StripHTML: true,
		StripBase64: true, StripURLTracking: true, CollapseWhitespace: true,
	})
	if label := messageTypeLabel(context.MessageType); label != "" {
		if text != "" {
			text += "\n\n"
		}
		text += label
	}
	bounded, capTruncated := truncateRunes(text, maxChars)
	return bounded, preprocessingTruncated || capTruncated
}

func truncateRunes(value string, maxRunes int) (string, bool) {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value, false
	}
	seen := 0
	for offset := range value {
		if seen == maxRunes {
			return value[:offset], true
		}
		seen++
	}
	return value, false
}

func messageTypeLabel(messageType string) string {
	switch strings.ToLower(strings.TrimSpace(messageType)) {
	case "slack", "discord", "beeper", "teams", "imessage", "whatsapp", "chat":
		return "Message type: chat message"
	case "mms":
		return "Message type: multimedia message"
	default:
		return ""
	}
}

func publishedRevision(request AssembleRequest, contextText string) string {
	hash := sha256.New()
	writeRevisionField(hash, request.Policy.InputVersion)
	writeRevisionField(hash, request.Policy.EligibilityVersion)
	writeRevisionField(hash, string(request.Role))
	writeRevisionField(hash, contextText)
	writeRevisionField(hash, request.Owner.BlobHash)
	writeRevisionField(hash, request.Owner.MediaInputKey)
	writeRevisionField(hash, request.Media.Kind)
	writeRevisionField(hash, request.Media.MIMEType)
	writeRevisionField(hash, request.Media.BlobHash)
	writeRevisionField(hash, strconv.FormatInt(request.Media.Width, 10))
	writeRevisionField(hash, strconv.FormatInt(request.Media.Height, 10))
	writeRevisionField(hash, strconv.FormatInt(request.Media.DurationMS, 10))
	writeRevisionField(hash, strconv.FormatBool(request.Media.Animated))
	return hex.EncodeToString(hash.Sum(nil))
}

type revisionWriter interface {
	Write(data []byte) (int, error)
}

func writeRevisionField(writer revisionWriter, value string) {
	_, _ = writer.Write([]byte(strconv.Itoa(len(value))))
	_, _ = writer.Write([]byte{':'})
	_, _ = writer.Write([]byte(value))
}
