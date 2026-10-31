// Package synctechstate classifies the numeric send states in Synctech backups.
package synctechstate

// RawFormat identifies the preserved Synctech provider record.
const RawFormat = "synctech_sms_xml_json"

// Numeric states are the values written by Synctech's backup format.
const (
	SMSTypeInbox  = 1
	SMSTypeSent   = 2
	SMSTypeDraft  = 3
	SMSTypeOutbox = 4
	SMSTypeFailed = 5
	SMSTypeQueued = 6

	MMSBoxInbox  = 1
	MMSBoxSent   = 2
	MMSBoxDraft  = 3
	MMSBoxOutbox = 4
)

// SMSWasSent excludes draft, outbox, failed, and queued records.
func SMSWasSent(kind int) bool {
	switch kind {
	case SMSTypeDraft, SMSTypeOutbox, SMSTypeFailed, SMSTypeQueued:
		return false
	default:
		return true
	}
}

// MMSWasSent excludes draft and outbox records.
func MMSWasSent(box int) bool {
	switch box {
	case MMSBoxDraft, MMSBoxOutbox:
		return false
	default:
		return true
	}
}
