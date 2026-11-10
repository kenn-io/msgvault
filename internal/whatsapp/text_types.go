package whatsapp

import "time"

// TextOptions supplies interpretations absent from a per-chat TXT export.
type TextOptions struct {
	Phone, ChatID, ChatType, ChatTitle string
	DateOrder, Timezone                string
	MediaDir, AttachmentsDir           string
	MaxMediaFileSize                   int64
}

// TextSummary counts evidence in this file, not the chat's lifetime history.
type TextSummary struct {
	SourceID                                                              int64
	Records, Notices, Added, Unchanged, DuplicateAmbiguity                int
	MediaStored, MediaMissing, MediaOmitted, MediaSkipped, MediaOversized int
}

type textRecord struct {
	Timestamp string    `json:"original_timestamp"`
	WallTime  string    `json:"wall_time"`
	Precision string    `json:"precision"`
	Sender    string    `json:"sender_label"`
	Body      string    `json:"body"`
	Kind      string    `json:"kind"`
	Raw       string    `json:"raw"`
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	SentAt    time.Time `json:"interpreted_sent_at"`
}

type textLimits struct {
	Bytes                int64
	Records, RecordBytes int
}

var textLimitsDefault = textLimits{128 * 1024 * 1024, 250000, 1024 * 1024}
