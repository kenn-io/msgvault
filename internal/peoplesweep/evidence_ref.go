package peoplesweep

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const personSweepEvidenceRefPrefix = "person-sweep/v1:"

func EncodePersonSweepEvidenceRef(ref EvidenceRef) (string, error) {
	if err := validateEvidenceRef(ref); err != nil {
		return "", err
	}
	payload, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("encode person sweep evidence ref: %w", err)
	}
	return personSweepEvidenceRefPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func DecodePersonSweepEvidenceRef(sourceRef string) (EvidenceRef, error) {
	var ref EvidenceRef
	if !strings.HasPrefix(sourceRef, personSweepEvidenceRefPrefix) {
		return ref, errors.New("person sweep evidence ref has unknown version")
	}
	encoded := strings.TrimPrefix(sourceRef, personSweepEvidenceRefPrefix)
	if encoded == "" || strings.Contains(encoded, "=") {
		return ref, errors.New("person sweep evidence ref is not unpadded base64url")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ref, fmt.Errorf("decode person sweep evidence ref: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ref); err != nil {
		return EvidenceRef{}, fmt.Errorf("decode person sweep evidence ref JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return EvidenceRef{}, errors.New("person sweep evidence ref has trailing JSON")
	}
	if err := validateEvidenceRef(ref); err != nil {
		return EvidenceRef{}, err
	}
	canonical, err := EncodePersonSweepEvidenceRef(ref)
	if err != nil {
		return EvidenceRef{}, err
	}
	if canonical != sourceRef {
		return EvidenceRef{}, errors.New("person sweep evidence ref is noncanonical")
	}
	return ref, nil
}

func validateEvidenceRef(ref EvidenceRef) error {
	if ref.SourceID <= 0 || ref.MessageID <= 0 {
		return errors.New("person sweep evidence ref requires source and message IDs")
	}
	if ref.SpanStart < 0 || ref.SpanEnd < ref.SpanStart {
		return errors.New("person sweep evidence ref has invalid rune span")
	}
	if !utf8.ValidString(ref.SourceMessageID) || !utf8.ValidString(ref.OccurrenceKey) || !utf8.ValidString(ref.ChunkKey) {
		return errors.New("person sweep evidence ref requires valid UTF-8 coordinates")
	}
	switch ref.SourceLane {
	case SourceConversationText, SourceMeetingText:
		if ref.AttachmentID != 0 || ref.OccurrenceKey != "" || ref.ChunkKey != "" {
			return errors.New("message evidence ref has document coordinates")
		}
	case SourceDocumentText:
		if ref.AttachmentID <= 0 || strings.TrimSpace(ref.OccurrenceKey) == "" || strings.TrimSpace(ref.ChunkKey) == "" {
			return errors.New("document evidence ref requires attachment, occurrence, and chunk")
		}
	case SourceAttachmentCaption, SourceAttachmentOCR:
		if ref.AttachmentID <= 0 || ref.OccurrenceKey != "" || ref.ChunkKey != "" {
			return errors.New("attachment evidence ref has invalid coordinates")
		}
	default:
		return fmt.Errorf("person sweep evidence ref has unknown lane %q", ref.SourceLane)
	}
	return nil
}

// ValidatePersonSweepEvidenceItem verifies that host-assigned evidence trust
// fields agree with the versioned archive coordinate before an item crosses a
// retrieval boundary.
func ValidatePersonSweepEvidenceItem(item EvidenceItem) error {
	if item.PersonID <= 0 || item.SourceClass != item.Ref.SourceLane {
		return errors.New("person sweep evidence item disagrees with its source lane")
	}
	if _, err := EncodePersonSweepEvidenceRef(item.Ref); err != nil {
		return err
	}
	if item.Tombstone {
		if item.Excerpt != "" || item.ContentSHA256 != "" {
			return errors.New("person sweep tombstone contains source text")
		}
		if strings.TrimSpace(item.EvidenceKey) == "" || strings.TrimSpace(item.SourceVersion) == "" {
			return errors.New("person sweep tombstone requires prior evidence identity")
		}
		return nil
	}
	if item.Highlight.Start != item.Ref.SpanStart || item.Highlight.End != item.Ref.SpanEnd {
		return errors.New("person sweep evidence highlight disagrees with its source ref")
	}
	if item.Highlight.End > utf8.RuneCountInString(item.Excerpt) {
		return errors.New("person sweep evidence highlight exceeds excerpt")
	}
	return nil
}
