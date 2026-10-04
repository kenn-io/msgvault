package kataissues

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/textutil"
)

// EvidenceMetadataKey holds the Envelope on a Kata issue.
const EvidenceMetadataKey = "msgvault.evidence"

const (
	maxTitleChars = 512
	maxBriefChars = 2000
)

// Envelope records which evidence an issue cites, in the order it was added. The readable quotations
// live in the issue body and comments. Each reference names its own archive.
type Envelope struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Entry keeps the full reference for provenance. Passage is what decides
// whether two citations quote the same thing: where the text sits and its
// words, without the hashes that change when the source is synced again.
type Entry struct {
	ID        string                 `json:"id"`
	Passage   string                 `json:"passage"`
	Reference kataevidence.Reference `json:"reference"`
	// Pending holds a new passage's quote until its comment is posted, so a
	// retry can post it without reading the source again.
	Pending *Pending `json:"pending,omitzero"`
}

// Pending is the quote a comment still owes the issue.
type Pending struct {
	Quote string `json:"quote"`
	Label string `json:"label"`
}

// quotation is a resolved entry with the text and label it is rendered with.
type quotation struct {
	Entry

	Snapshot, Label string
}

// parseEnvelope validates issue metadata before anything edits it.
func parseEnvelope(value any) (Envelope, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrUnsupportedEvidence, err)
	}
	var envelope Envelope
	if err := json.Unmarshal(encoded, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrUnsupportedEvidence, err)
	}
	return validateEnvelope(envelope)
}

func validateEnvelope(envelope Envelope) (Envelope, error) {
	if envelope.Version != 1 || len(envelope.Entries) < 1 {
		return Envelope{}, ErrUnsupportedEvidence
	}
	envelope.Entries = slices.Clone(envelope.Entries)
	seen := make(map[string]bool, len(envelope.Entries))
	for i := range envelope.Entries {
		entry := &envelope.Entries[i]
		canonical, err := kataevidence.Canonicalize(entry.Reference)
		if err != nil || !hexDigest(entry.Passage) || entry.Pending != nil && (!boundedText(entry.Pending.Quote, kataevidence.MaxChars) || !boundedText(entry.Pending.Label, maxTitleChars)) {
			return Envelope{}, ErrUnsupportedEvidence
		}
		id := kataevidence.ID(canonical)
		// A pending quote must still be the words its passage recorded.
		if entry.ID != id || seen[id] || entry.Pending != nil && kataevidence.PassageID(canonical, entry.Pending.Quote) != entry.Passage {
			return Envelope{}, ErrUnsupportedEvidence
		}
		entry.Reference = canonical
		seen[id] = true
	}
	return envelope, nil
}

func boundedText(value string, maximum int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum
}

// renderBody quotes every snapshot verbatim so the issue reads on its own.
func renderBody(brief string, quotes []quotation) string {
	var out strings.Builder
	if brief != "" {
		out.WriteString(brief)
		out.WriteString("\n\n")
	}
	for _, quote := range quotes {
		out.WriteString(renderQuote(quote))
		out.WriteByte('\n')
	}
	return out.String()
}

// renderQuote quotes one snapshot under its source line.
func renderQuote(quote quotation) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Evidence: %s\n\n", strings.ReplaceAll(quote.Label, "\n", " "))
	for line := range strings.SplitSeq(quote.Snapshot, "\n") {
		out.WriteString("> ")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func hexDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// label names a quote's source, bounded so a pending entry that stores it
// always passes validateEnvelope.
func label(display kataevidence.Display) string {
	name := cmp.Or(display.Filename, display.ContainingTitle, "Archived message")
	return textutil.TruncateRunes(strings.ToValidUTF8(name, "�"), maxTitleChars)
}
