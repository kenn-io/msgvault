// Package sendstate decides whether a Synctech text ever left the phone. The
// importer reads the backup's status at import time and the store migration
// reads it from the retained raw record, so both ask this one place. It has no
// dependencies because the store cannot import the importer.
package sendstate

// Android SMS type and MMS message box values for texts the owner wrote that
// were not delivered.
const (
	SMSDraft  = 3
	SMSOutbox = 4
	SMSFailed = 5
	SMSQueued = 6

	MMSDraft  = 3
	MMSOutbox = 4
)

// SMSUnsent reports whether an SMS of this type never left the phone.
func SMSUnsent(smsType int) bool {
	switch smsType {
	case SMSDraft, SMSOutbox, SMSFailed, SMSQueued:
		return true
	}
	return false
}

// MMSUnsent reports whether an MMS in this message box never left the phone.
func MMSUnsent(box int) bool {
	return box == MMSDraft || box == MMSOutbox
}
