// Package kataevidence prepares and resolves exact citations of archived
// message bodies and extracted document chunks. It never starts document
// processing or follows source URLs.
package kataevidence

import (
	"errors"
	"fmt"
	"time"
)

const (
	Version = 1
	// MaxChars bounds every citation: the prepared window, a valid reference
	// and the saved quotation. Longer references are rejected, never shortened.
	MaxChars      = 1000
	MaxReferences = 32
	// MaxDocbankReferences keeps 4 x DocbankTimeout inside the 30s client timeout.
	MaxDocbankReferences = 4
	// MaxRequestBytes fits MaxReferences quotes of MaxChars runes even when
	// every rune is JSON-escaped, up to 12 bytes for a surrogate pair, plus the
	// rest of the request.
	MaxRequestBytes = 512 << 10
	// ContextRunes is how much text a read shows on each side of a citation.
	ContextRunes = 500
)

var (
	ErrInvalidReference = errors.New("invalid Kata evidence reference")
	ErrDocbankLimit     = fmt.Errorf("a request can cite at most %d Docbank transcript references", MaxDocbankReferences)
	ErrUnavailable      = errors.New("kata evidence unavailable")
	ErrChanged          = errors.New("kata evidence changed")
	ErrUnprocessed      = errors.New("kata evidence unprocessed")
	ErrUnsupported      = errors.New("kata evidence unsupported")
	ErrQuoteNotFound    = errors.New("kata evidence quote not found in the source")
	ErrQuoteAmbiguous   = errors.New("kata evidence quote appears more than once in the source")
	// ErrArchiveUnavailable wraps a failure to read the archive itself.
	ErrArchiveUnavailable = errors.New("archive is unavailable")
	// ErrDocbankUnavailable marks a failure from the Docbank client.
	ErrDocbankUnavailable = fmt.Errorf("docbank is unavailable: %w", ErrArchiveUnavailable)
)

// Reference names one exact range of one archived representation.
type Reference struct {
	Version          int                `json:"version"`
	Kind             string             `json:"kind" enum:"message,document_chunk,docbank_rendition"`
	ArchiveUID       string             `json:"archive_uid"`
	MessageID        int64              `json:"message_id"`
	SourceType       string             `json:"source_type"`
	SourceIdentifier string             `json:"source_identifier"`
	SourceMessageID  string             `json:"source_message_id"`
	AttachmentID     int64              `json:"attachment_id,omitzero"`
	OccurrenceKey    string             `json:"occurrence_key,omitzero"`
	Message          *MessageReference  `json:"message,omitzero"`
	DocumentChunk    *DocumentReference `json:"document_chunk,omitzero"`
	DocbankRendition *DocbankReference  `json:"docbank_rendition,omitzero"`
}

type MessageReference struct {
	BodySHA256 string `json:"body_sha256"`
	StartRune  int    `json:"start_rune"`
	EndRune    int    `json:"end_rune"`
}

type DocumentReference struct {
	CanonicalBlobHash string `json:"canonical_blob_hash"`
	ExtractionID      string `json:"extraction_id"`
	ManifestChecksum  string `json:"manifest_checksum"`
	ChunkKey          string `json:"chunk_key"`
	ChunkChecksum     string `json:"chunk_checksum"`
	StartRune         int    `json:"start_rune"`
	EndRune           int    `json:"end_rune"`
}

// DocbankReference pins one window of the selected rendition, such as a
// transcript, of a file msgvault delivered to Docbank.
type DocbankReference struct {
	VaultUID              string `json:"vault_uid"`
	NodeID                int64  `json:"node_id"`
	ContentVersionID      string `json:"content_version_id"`
	ContentSHA256         string `json:"content_sha256"`
	RenditionAttachmentID string `json:"rendition_attachment_id"`
	BuildID               string `json:"build_id"`
	RenditionSHA256       string `json:"rendition_sha256"`
	StartRune             int    `json:"start_rune"`
	EndRune               int    `json:"end_rune"`
}

// DocbankBinding is the retained Docbank delivery of one attachment: its
// source tuple and occurrence key in Reference, and Docbank's identities.
type DocbankBinding struct {
	Reference        Reference
	VaultUID         string
	ContentVersionID string
	ContentSHA256    string
	Profile          string
	Display          Display
}

// Selector identifies a stored occurrence and either an exact range or a
// window of at most MaxChars runes. The daemon derives all other provenance.
type Selector struct {
	Kind         string `json:"kind" enum:"message,document_chunk,docbank_rendition"`
	MessageID    int64  `json:"message_id"`
	AttachmentID int64  `json:"attachment_id,omitzero"`
	ExtractionID string `json:"extraction_id,omitzero"`
	ChunkKey     string `json:"chunk_key,omitzero"`
	StartRune    *int   `json:"start_rune,omitzero"`
	EndRune      *int   `json:"end_rune,omitzero"`
	MaxChars     *int   `json:"max_chars,omitzero"`
	// Quote selects the one place this exact text appears, instead of a range.
	Quote string `json:"quote,omitzero" maxLength:"1000"`
}

type Display struct {
	Filename        string    `json:"filename,omitzero"`
	ContainingTitle string    `json:"containing_title,omitzero"`
	Timestamp       time.Time `json:"timestamp,omitzero"`
}

// SourceRecord is the text of one representation, or of the window of it
// that starts at rune TextStart.
type SourceRecord struct {
	Reference Reference
	Text      string
	TextStart int
	// More reports that the representation goes on past Text.
	More    bool
	Display Display
}

type State string

const (
	Available   State = "available"
	Changed     State = "changed"
	Unavailable State = "unavailable"
	Unprocessed State = "unprocessed"
	Unsupported State = "unsupported"
	// Unreachable means Docbank couldn't be reached to read the passage; retry later.
	Unreachable State = "unreachable"
)

// Evidence is one prepared citation. Excerpt is exactly the text a Kata
// issue saves for Reference.
type Evidence struct {
	ID string `json:"id"`
	// Passage identifies the quoted words where they sit; it survives re-syncs
	// that change only content hashes, so clients key retries on it.
	Passage   string    `json:"passage"`
	Reference Reference `json:"reference"`
	Excerpt   string    `json:"excerpt"`
	Display   Display   `json:"display"`
	// NextRune is where the following window starts; zero means the source ends here.
	NextRune     int    `json:"next_rune,omitzero"`
	ContentTrust string `json:"content_trust"`
}

type Resolution struct {
	State    State    `json:"state"`
	Evidence Evidence `json:"evidence"`
	// Before and After hold up to ContextRunes of the text around an
	// available excerpt when the caller asks for them.
	Before string `json:"before,omitzero" doc:"Up to 500 characters of source text before the excerpt, when available"`
	After  string `json:"after,omitzero" doc:"Up to 500 characters of source text after the excerpt, when available"`
}

// Range returns the half-open rune range of a validated reference.
func (r Reference) Range() (int, int) {
	switch {
	case r.Kind == "message" && r.Message != nil:
		return r.Message.StartRune, r.Message.EndRune
	case r.Kind == "document_chunk" && r.DocumentChunk != nil:
		return r.DocumentChunk.StartRune, r.DocumentChunk.EndRune
	case r.Kind == "docbank_rendition" && r.DocbankRendition != nil:
		return r.DocbankRendition.StartRune, r.DocbankRendition.EndRune
	}
	return 0, 0
}
