// Package sendstate names the Android SMS types and MMS boxes in a Synctech
// backup and says which texts the owner wrote and which never left the phone.
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

// SMSUnsent and MMSUnsent report drafts, outbox, queued, and failed texts.
func SMSUnsent(t SMSType) bool {
	return t == SMSTypeDraft || t == SMSTypeOutbox || t == SMSTypeFailed || t == SMSTypeQueued
}

func MMSUnsent(box MMSBox) bool {
	return box == MMSBoxDraft || box == MMSBoxOutbox || box == MMSBoxFailed
}

// SMSFromOwner and MMSFromOwner report texts the owner wrote, sent or not.
func SMSFromOwner(t SMSType) bool { return t == SMSTypeSent || SMSUnsent(t) }

func MMSFromOwner(box MMSBox) bool { return box == MMSBoxSent || MMSUnsent(box) }
