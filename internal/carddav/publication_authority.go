package carddav

import (
	"bytes"
	"context"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

type publicationAuthorityKey struct{}

type publicationRequestAuthority struct {
	service   *Service
	expected  store.CardDAVPublication
	authorize store.PersonEditAuthorizer
}

// withPublicationRequestAuthority binds authority to this operation only.
func (s *Service) withPublicationRequestAuthority(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) context.Context {
	return context.WithValue(ctx, publicationAuthorityKey{}, publicationRequestAuthority{s, clonePublicationIntent(expected), authorize})
}

func clonePublicationIntent(expected store.CardDAVPublication) store.CardDAVPublication {
	expected.OutgoingBody = slices.Clone(expected.OutgoingBody)
	expected.OutgoingEnvelopeMetadata = slices.Clone(expected.OutgoingEnvelopeMetadata)
	if expected.ApprovedBodySHA256 != nil {
		value := *expected.ApprovedBodySHA256
		expected.ApprovedBodySHA256 = &value
	}
	if expected.ApprovedInferenceRevision != nil {
		value := *expected.ApprovedInferenceRevision
		expected.ApprovedInferenceRevision = &value
	}
	if expected.ApprovedMutationRevision != nil {
		value := *expected.ApprovedMutationRevision
		expected.ApprovedMutationRevision = &value
	}
	if expected.PendingStartedAt != nil {
		value := *expected.PendingStartedAt
		expected.PendingStartedAt = &value
	}
	return expected
}

func authorizePublicationDispatch(ctx context.Context, method, target string, request Request) error {
	authority, scoped := ctx.Value(publicationAuthorityKey{}).(publicationRequestAuthority)
	if !scoped {
		return nil
	}
	if authority.service == nil || authority.service.store == nil || authority.authorize == nil {
		return store.ErrCardDAVInvalidPlan
	}
	if method == http.MethodPut && (request.Create || request.ETag != authority.expected.RemoteETag || !bytes.Equal(request.Body, authority.expected.OutgoingBody)) {
		return store.ErrCardDAVInvalidPlan
	}
	if err := authority.service.requireOwnBook(ctx, authority.expected.AddressBookID); err != nil {
		return err
	}
	return authority.service.store.AuthorizeCardDAVPublicationRequestContext(ctx, authority.expected, method, target, authority.authorize)
}
