// Package sendstate holds the Android SMS types and MMS boxes a Synctech backup
// records, and decides which texts the owner wrote and which ever left the
// phone. The importer and the store migration share it.
package sendstate

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
	MMSBoxFailed MMSBox = 5
)

// SMSUnsent reports whether a text is a draft, outbox, failed, or queued one.
func SMSUnsent(t SMSType) bool {
	return t == SMSTypeDraft || t == SMSTypeOutbox || t == SMSTypeFailed || t == SMSTypeQueued
}

// MMSUnsent reports whether a message is in the drafts, outbox, or failed box.
func MMSUnsent(box MMSBox) bool {
	return box == MMSBoxDraft || box == MMSBoxOutbox || box == MMSBoxFailed
}

// SMSFromOwner reports whether the owner wrote a text: sent or never sent.
func SMSFromOwner(t SMSType) bool { return t == SMSTypeSent || SMSUnsent(t) }

// MMSFromOwner reports whether the owner wrote a message: sent or never sent.
func MMSFromOwner(box MMSBox) bool { return box == MMSBoxSent || MMSUnsent(box) }
