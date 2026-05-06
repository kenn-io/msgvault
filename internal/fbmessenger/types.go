package fbmessenger

import (
	"errors"
	"time"
)

// ErrCorruptJSON is returned by the JSON parser when a message_*.json file
// cannot be parsed as valid JSON. Callers should log and skip the thread.
var ErrCorruptJSON = errors.New("fbmessenger: corrupt json")

// ErrNotE2EEThread is returned by ParseE2EEJSONFile when a candidate
// flat-export JSON file parses successfully but is not an E2EE thread
// (e.g. a DYI metadata file). Callers should silently skip — the
// discoverer does not pre-filter by shape, so the parser is responsible
// for rejecting non-thread JSON.
var ErrNotE2EEThread = errors.New("fbmessenger: not an e2ee thread")

// Thread is the parsed form of one DYI thread directory, ready for import.
type Thread struct {
	// DirName is the directory name (e.g. "testuser_ABC123XYZ"). This
	// is used verbatim as source_conversation_id.
	DirName string
	// Section is the DYI section ("inbox", "archived_threads", ...).
	// Populated by the importer from ThreadDir.Section.
	Section string
	// Title is the human-readable thread title (may be empty).
	Title string
	// ConvType is "direct_chat" for 2 participants or "group_chat" for 3+.
	ConvType string
	// Participants are every participant in the thread (including the
	// importing user).
	Participants []Participant
	// Messages are sorted chronologically (ascending).
	Messages []Message
	// Format is "json" or "html".
	Format string
	// RawBytes is the original source bytes for round-tripping into
	// message_raw. For multi-file JSON threads we concatenate files into
	// a single JSON array for storage; HTML threads store the single file.
	RawBytes []byte
	// BadSiblings holds the names of sibling files in the thread directory
	// that looked like `message_*.json` but did not match the expected
	// `message_<N>.json` pattern. The parser skips them and records them
	// here so the importer can log and count them without aborting the
	// rest of the thread.
	BadSiblings []string
}

// Participant is a thread participant from the DYI export.
type Participant struct {
	Name string
}

// Attachment represents a single file referenced by a message.
type Attachment struct {
	// URI is the relative path from the DYI export root.
	URI string
	// AbsPath is the absolute filesystem path after resolution; empty
	// when the file is missing or the URI escapes the root.
	AbsPath string
	// Kind is "photo", "video", "audio", "file", "gif", or "sticker".
	Kind string
	// MimeType is a best-guess content type; may be empty.
	MimeType string
	// Filename is the base file name from the URI.
	Filename string
}

// Reaction is a reaction attached to a message.
type Reaction struct {
	Actor    string
	Reaction string
}

// Message is one message in a parsed DYI thread.
type Message struct {
	// Index is a monotonic per-thread index (0-based) used to construct
	// a stable source_message_id.
	Index int
	// SenderName is the raw display name reported by DYI. Empty for
	// unknown / system messages.
	SenderName string
	// SentAt is the message timestamp in UTC. Zero when we could not parse.
	SentAt time.Time
	// Body is the rendered message body. Placeholders like "[sticker]" or
	// "[call: 3m 12s]" are produced for non-text messages per plan D10.
	Body string
	// Attachments are any files referenced from this message.
	Attachments []Attachment
	// Reactions are reactions applied to this message.
	Reactions []Reaction
	// Type is the DYI "type" string (e.g. "Generic", "Share", "Call",
	// "Unsubscribe"). Empty for HTML imports.
	Type string
}
