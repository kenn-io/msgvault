package gvoice

import "time"

// fileType classifies a Google Voice Takeout HTML file.
type fileType int

const (
	fileTypeText      fileType = iota // SMS/MMS conversation
	fileTypeReceived                  // Received call
	fileTypePlaced                    // Placed call
	fileTypeMissed                    // Missed call
	fileTypeVoicemail                 // Voicemail
	fileTypeGroup                     // Group conversation
)

func (ft fileType) String() string {
	switch ft {
	case fileTypeText:
		return "text"
	case fileTypeReceived:
		return "received"
	case fileTypePlaced:
		return "placed"
	case fileTypeMissed:
		return "missed"
	case fileTypeVoicemail:
		return "voicemail"
	case fileTypeGroup:
		return "group"
	default:
		return "unknown"
	}
}

// labelForFileType returns the label string used in ListLabels and message labels.
func labelForFileType(ft fileType) string {
	switch ft {
	case fileTypeText:
		return "sms"
	case fileTypeGroup:
		return "sms"
	case fileTypeReceived:
		return "call_received"
	case fileTypePlaced:
		return "call_placed"
	case fileTypeMissed:
		return "call_missed"
	case fileTypeVoicemail:
		return "voicemail"
	default:
		return "unknown"
	}
}

// ownerPhones holds phone numbers parsed from Phones.vcf.
type ownerPhones struct {
	GoogleVoice string // Google Voice number (e.g., +17026083638)
	Cell        string // Cell number (e.g., +15753222266)
}

// indexEntry is a pre-indexed reference to a single message or call record
// within a Takeout HTML file. One HTML file may contain many messages.
type indexEntry struct {
	ID           string // deterministic dedup ID (sha256-based)
	ThreadID     string // conversation grouping key
	FilePath     string // path to HTML file
	MessageIndex int    // index within the HTML file (for text files with multiple messages)
	Timestamp    time.Time
	FileType     fileType
	Labels       []string // e.g., ["sms", "inbox"]
}

// textMessage is a parsed individual SMS/MMS from a text conversation HTML file.
type textMessage struct {
	Timestamp   time.Time
	SenderPhone string
	SenderName  string
	Body        string
	Attachments []attachmentRef
	IsMe        bool // true if sender is the device owner
}

// attachmentRef references an MMS attachment found in the HTML.
type attachmentRef struct {
	HrefInHTML string // href attribute value (no extension in HTML)
	MediaType  string // "video", "image", etc.
}

// callRecord is a parsed call log entry.
type callRecord struct {
	CallType  fileType // received, placed, missed, voicemail
	Phone     string   // contact phone number
	Name      string   // contact display name
	Timestamp time.Time
	Duration  string   // ISO 8601 duration (e.g., "PT1M23S")
	Labels    []string // from the HTML tags section
	// AudioSrc is the raw src attribute of the audio element inside the
	// .haudio subtree. It is empty when the export names no audio.
	AudioSrc string
}

// ImportSummary holds statistics from a completed import run.
type ImportSummary struct {
	MessagesImported      int
	ConversationsImported int
	ParticipantsResolved  int
	Skipped               int
}

// MessageTypeForFileType maps a Google Voice file type to the
// message_type string stored in the database.
func MessageTypeForFileType(ft fileType) string {
	switch ft {
	case fileTypeText, fileTypeGroup:
		return "google_voice_text"
	case fileTypeReceived, fileTypePlaced, fileTypeMissed:
		return "google_voice_call"
	case fileTypeVoicemail:
		return "google_voice_voicemail"
	default:
		return "google_voice_text"
	}
}

// voicemailAudioPartKey identifies the recording occurrence for a voicemail.
const voicemailAudioPartKey = "gvoice:voicemail:audio"
