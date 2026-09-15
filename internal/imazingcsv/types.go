package imazingcsv

import "time"

// Direction is the normalized iMazing message direction.
type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

// Layout describes the resolved directories and CSV files in one export.
type Layout struct {
	Root           string
	CSVDir         string
	AttachmentsDir string
	CSVFiles       []string
}

// Row is one normalized physical CSV record. Raw preserves every named value.
type Row struct {
	ChatSession    string
	MessageDate    string
	DeliveredDate  string
	ReadDate       string
	Service        string
	SenderID       string
	SenderName     string
	Status         string
	ReplyingTo     string
	Subject        string
	Text           string
	Attachment     string
	AttachmentType string
	Direction      Direction
	SentAt         time.Time
	DeliveredAt    *time.Time
	ReadAt         *time.Time
	Raw            map[string]string
	File           string
	Record         int
}
