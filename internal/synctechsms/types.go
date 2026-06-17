package synctechsms

import (
	"database/sql"
	"time"
)

const (
	AdapterName               = "synctech-sms"
	SourceType                = "synctech_sms"
	ParticipantIdentifierType = "synctech_sms"
	RawFormat                 = "synctech_sms_xml_json"
)

type BackupKind string

const (
	KindMessages BackupKind = "messages"
	KindCalls    BackupKind = "calls"
)

type SMSType int

const (
	SMSTypeInbox  SMSType = 1
	SMSTypeSent   SMSType = 2
	SMSTypeDraft  SMSType = 3
	SMSTypeOutbox SMSType = 4
	SMSTypeFailed SMSType = 5
	SMSTypeQueued SMSType = 6
)

type MMSBox int

const (
	MMSBoxInbox  MMSBox = 1
	MMSBoxSent   MMSBox = 2
	MMSBoxDraft  MMSBox = 3
	MMSBoxOutbox MMSBox = 4
)

type MMSAddressType int

const (
	MMSAddressBCC  MMSAddressType = 129
	MMSAddressCC   MMSAddressType = 130
	MMSAddressFrom MMSAddressType = 137
	MMSAddressTo   MMSAddressType = 151
)

type CallType int

const (
	CallIncoming  CallType = 1
	CallOutgoing  CallType = 2
	CallMissed    CallType = 3
	CallVoicemail CallType = 4
	CallRejected  CallType = 5
	CallRefused   CallType = 6
)

type Document struct {
	Kind       BackupKind
	Count      int
	BackupSet  string
	BackupDate time.Time
	SMS        []SMS
	MMS        []MMS
	Calls      []Call
}

type RecordKind string

const (
	RecordSMS  RecordKind = "sms"
	RecordMMS  RecordKind = "mms"
	RecordCall RecordKind = "call"
)

type Record struct {
	Kind RecordKind
	SMS  *SMS
	MMS  *MMS
	Call *Call
}

type SMS struct {
	Protocol      string
	Address       string
	Timestamp     time.Time
	Type          SMSType
	Subject       sql.NullString
	Body          string
	ServiceCenter sql.NullString
	Read          bool
	Status        int
	SubID         sql.NullString
	ContactName   sql.NullString
	RawAttrs      map[string]string
}

type MMS struct {
	Timestamp   time.Time
	MessageBox  MMSBox
	Address     sql.NullString
	MessageID   sql.NullString
	Subject     sql.NullString
	ContentType sql.NullString
	Read        bool
	ContactName sql.NullString
	Parts       []MMSPart
	Addresses   []MMSAddress
	RawAttrs    map[string]string
}

type MMSPart struct {
	Sequence    int
	ContentType string
	Name        sql.NullString
	Filename    sql.NullString
	Charset     sql.NullString
	Text        sql.NullString
	Data        []byte
	RawAttrs    map[string]string
}

type MMSAddress struct {
	Address string
	Type    MMSAddressType
	Charset sql.NullString
}

type Call struct {
	Number          string
	DurationSeconds int
	Timestamp       time.Time
	Type            CallType
	Presentation    int
	SubscriptionID  sql.NullString
	ContactName     sql.NullString
	RawAttrs        map[string]string
}

type ImportOptions struct {
	OwnerPhone         string
	AttachmentsDir     string
	MaxAttachmentBytes int64
	IncludeSMS         bool
	IncludeMMS         bool
	IncludeCalls       bool
	IncludeAttachments bool
	NoResume           bool
}

type ImportSummary struct {
	FilesSeen           int
	FilesImported       int
	FilesSkipped        int
	SMSImported         int
	MMSImported         int
	CallsImported       int
	AttachmentsImported int
}
