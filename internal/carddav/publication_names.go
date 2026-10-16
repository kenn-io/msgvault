package carddav

import (
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

// adoptLegacyPublicationFullName retires the old, unmapped display-name seed
// before the projector supplies its owned replacement. Historical cards have
// no seed marker, so adoption is limited to a settled local publication with
// no imported-name history and a bare first FN matching the original card.
func adoptLegacyPublicationFullName(source *store.CardDAVPublicationReviewSource, envelope vcard.ResourceEnvelope) (vcard.ResourceEnvelope, error) {
	publication, resource := source.Publication, source.Resource
	if publication == nil || !publication.Desired || publication.PendingOperation != "" ||
		resource == nil || resource.Governance != store.CardDAVGovernanceLocal ||
		!resource.LocallyPublished ||
		publication.PersonID != source.Person.ID || publication.AddressBookID != source.Book.ID ||
		publication.Href != resource.Href || source.HasImportedNameHistory {
		return envelope, nil
	}
	seed, ok, err := store.LegacyCardDAVPublicationFullName(envelope, envelope.OriginalRawBytes)
	if err != nil || !ok {
		return envelope, err
	}
	return envelope.MergeProperties([]vcard.PropertyEdit{{Identity: seed.Identity, Delete: true}})
}
