// Package sendstate decides whether a Synctech text ever left the phone, for
// the importer and for the store migration, which cannot import the importer.
package sendstate

// SMSUnsent reports whether an Android SMS type is a draft (3), outbox (4),
// failed (5), or queued (6) text.
func SMSUnsent(smsType int) bool {
	return smsType >= 3 && smsType <= 6
}

// MMSUnsent reports whether an Android MMS message box is the drafts (3) or
// outbox (4).
func MMSUnsent(box int) bool {
	return box == 3 || box == 4
}
