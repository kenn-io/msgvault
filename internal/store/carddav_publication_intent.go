package store

import "bytes"

// cardDAVPublicationIntentMatches binds scoped completion and recovery to the
// original native intent. Recovery may advance book and mapping fences without
// changing this identity or its approved outgoing artifact.
func cardDAVPublicationIntentMatches(current *CardDAVPublication, expected CardDAVPublication) bool {
	return current.PendingIntentID != "" && current.PendingIntentID == expected.PendingIntentID &&
		current.PersonID == expected.PersonID && current.AddressBookID == expected.AddressBookID &&
		current.PendingOperation == expected.PendingOperation && current.MutationRevision == expected.MutationRevision &&
		current.Desired == expected.Desired && current.Href == expected.Href &&
		current.ConnectionGeneration == expected.ConnectionGeneration &&
		current.PendingStartedAt != nil && expected.PendingStartedAt != nil && current.PendingStartedAt.Equal(*expected.PendingStartedAt) &&
		bytes.Equal(current.OutgoingBody, expected.OutgoingBody) && current.OutgoingSemanticHash == expected.OutgoingSemanticHash &&
		bytes.Equal(current.OutgoingEnvelopeMetadata, expected.OutgoingEnvelopeMetadata) &&
		current.LocalHash == expected.LocalHash && current.RemoteETag == expected.RemoteETag &&
		cardDAVPublicationOptionalEqual(current.ApprovedBodySHA256, expected.ApprovedBodySHA256) &&
		cardDAVPublicationOptionalEqual(current.ApprovedInferenceRevision, expected.ApprovedInferenceRevision) &&
		cardDAVPublicationOptionalEqual(current.ApprovedMutationRevision, expected.ApprovedMutationRevision)
}

func cardDAVPublicationOptionalEqual[T comparable](left, right *T) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
