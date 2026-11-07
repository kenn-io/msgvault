package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

type PersonPhotoEvidence struct {
	AssertionID          string                  `json:"assertion_id"`
	ContentVersionID     string                  `json:"content_version_id"`
	Occurrences          []MediaAttachmentResult `json:"occurrences"`
	OccurrencesTruncated bool                    `json:"occurrences_truncated"`
}
type PersonPhotosResponse struct {
	UID        string                `json:"uid"`
	LinkState  string                `json:"link_state"`
	PersonID   string                `json:"person_id,omitempty"`
	Photos     []PersonPhotoEvidence `json:"photos"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Truncated  bool                  `json:"truncated"`
}
type PersonPhotoLinkRequest struct {
	UID       string `json:"uid" minLength:"1" maxLength:"256"`
	PersonID  string `json:"person_id" minLength:"1" maxLength:"128"`
	Revision  int64  `json:"revision" minimum:"1"`
	State     string `json:"state" enum:"current,unlinked"`
	LinkedUID string `json:"linked_uid,omitempty" maxLength:"256"`
}
type PersonPhotoLinkResponse struct {
	UID       string `json:"uid"`
	LinkedUID string `json:"linked_uid"`
	State     string `json:"state"`
	Revision  int64  `json:"revision"`
}

func (s *Server) photoReader() (*DocbankAttachmentReader, error) {
	if s.cfg == nil || !s.cfg.Integrations.Docbank.Enabled || !s.cfg.Integrations.Docbank.AttachmentUploadConsent || s.docbankAttachments == nil || s.docbankAttachments.Store == nil || s.docbankAttachments.Client == nil || s.docbankAttachments.Destination == "" {
		return nil, huma.Error503ServiceUnavailable("Docbank attachment integration unavailable")
	}
	return s.docbankAttachments, nil
}
func photoError(err error) error {
	var remote *docbankmedia.HTTPError
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		return huma.Error404NotFound("person not found")
	case errors.Is(err, store.ErrPersonUIDAmbiguous):
		return huma.Error409Conflict("person UID is ambiguous")
	case errors.As(err, &remote) && remote.Status == http.StatusConflict:
		return huma.Error409Conflict("Docbank identity conflict; review required")
	case errors.As(err, &remote) && remote.Status == http.StatusPreconditionFailed:
		return huma.Error412PreconditionFailed("Docbank person revision changed")
	default:
		return huma.Error503ServiceUnavailable("person photo evidence unavailable")
	}
}
func photoIdentity(ctx context.Context, r *DocbankAttachmentReader, uid string) (*store.Person, []string, string, error) {
	person, err := r.Store.GetPersonByUIDContext(ctx, uid)
	if err != nil {
		return nil, nil, "", err
	}
	uids, err := r.Store.ListPersonUIDsContext(ctx, person.ID)
	if err != nil {
		return nil, nil, "", err
	}
	if len(uids) == 0 || len(uids) > 64 {
		return nil, nil, "", huma.Error409Conflict("person has too many UIDs for photo lookup")
	}
	archive, err := r.Store.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	return person, uids, archive, nil
}
func photoRemotePerson(ctx context.Context, r *DocbankAttachmentReader, archive string, uids []string) (string, error) {
	var id string
	for _, uid := range uids {
		found, err := r.Client.ResolveExternalPerson(ctx, archive, uid)
		if docbankmedia.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if id != "" && id != found.PersonID {
			return "", huma.Error409Conflict("multiple Docbank people linked; review required")
		}
		id = found.PersonID
	}
	return id, nil
}
func photoFailure(err error) error {
	if status, ok := errors.AsType[huma.StatusError](err); ok {
		return status
	}
	return photoError(err)
}

func (s *Server) registerPersonPhotoRoutes(api huma.API) {
	get := rawAPIV1Operation("getPersonPhotos", http.MethodGet, "/people/by-uid/photos", "List confirmed person photo attachments")
	get.Errors = append(get.Errors, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	huma.Register(api, get, func(ctx context.Context, in *struct {
		UID    string `query:"uid" required:"true" minLength:"1" maxLength:"256"`
		Cursor string `query:"cursor" maxLength:"4096"`
	}) (*struct{ Body PersonPhotosResponse }, error) {
		r, err := s.photoReader()
		if err != nil {
			return nil, err
		}
		person, uids, archive, err := photoIdentity(ctx, r, in.UID)
		if err != nil {
			return nil, photoFailure(err)
		}
		id, err := photoRemotePerson(ctx, r, archive, uids)
		if err != nil {
			return nil, photoFailure(err)
		}
		response := PersonPhotosResponse{UID: person.VCardUID, LinkState: "unlinked", Photos: []PersonPhotoEvidence{}}
		if id == "" {
			again, latestUIDs, latestArchive, err := photoIdentity(ctx, r, in.UID)
			if err != nil {
				return nil, photoFailure(err)
			}
			if again.ID != person.ID || again.VCardUID != person.VCardUID || archive != latestArchive || !slices.Equal(uids, latestUIDs) {
				return nil, huma.Error409Conflict("person changed; retry")
			}
			return &struct{ Body PersonPhotosResponse }{response}, nil
		}
		page, err := r.Client.PersonAssertions(ctx, id, in.Cursor)
		if err != nil {
			return nil, photoFailure(err)
		}
		if page.State != "curated" && page.State != "provisional" {
			return nil, huma.Error409Conflict("Docbank person is not active")
		}
		again, latestUIDs, latestArchive, err := photoIdentity(ctx, r, in.UID)
		if err != nil {
			return nil, photoFailure(err)
		}
		linked, err := photoRemotePerson(ctx, r, latestArchive, latestUIDs)
		if err != nil {
			return nil, photoFailure(err)
		}
		if again.ID != person.ID || again.VCardUID != person.VCardUID || archive != latestArchive || linked != id || !slices.Equal(uids, latestUIDs) {
			return nil, huma.Error409Conflict("person link changed; retry")
		}
		// Assertions have their own revisions, so compare the page as well as the person revision.
		current, err := r.Client.PersonAssertions(ctx, id, in.Cursor)
		if err != nil {
			return nil, photoFailure(err)
		}
		if current.Revision != page.Revision || current.State != page.State || current.NextCursor != page.NextCursor || current.Truncated != page.Truncated || !slices.Equal(current.Assertions, page.Assertions) {
			return nil, huma.Error409Conflict("photo assertions changed; retry")
		}
		finalPerson, finalUIDs, finalArchive, err := photoIdentity(ctx, r, in.UID)
		if err != nil {
			return nil, photoFailure(err)
		}
		if finalPerson.ID != person.ID || finalPerson.VCardUID != person.VCardUID || finalArchive != archive || !slices.Equal(uids, finalUIDs) {
			return nil, huma.Error409Conflict("person changed; retry")
		}
		ids := make([]string, 0, len(page.Assertions))
		for _, a := range page.Assertions {
			if a.Role == "depicted" && a.Action == "assert" {
				ids = append(ids, a.ContentVersionID)
			}
		}
		occurrences, err := r.Store.PersonPhotoOccurrences(ctx, r.Destination, ids, 10)
		if err != nil {
			return nil, photoFailure(err)
		}
		byVersion := map[string][]MediaAttachmentResult{}
		truncated := map[string]bool{}
		for _, o := range occurrences {
			if len(byVersion[o.VersionID]) >= 10 {
				truncated[o.VersionID] = true
				continue
			}
			byVersion[o.VersionID] = append(byVersion[o.VersionID], MediaAttachmentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt})
		}
		for _, a := range page.Assertions {
			if a.Role == "depicted" && a.Action == "assert" && len(byVersion[a.ContentVersionID]) > 0 {
				response.Photos = append(response.Photos, PersonPhotoEvidence{AssertionID: a.AssertionID, ContentVersionID: a.ContentVersionID, Occurrences: byVersion[a.ContentVersionID], OccurrencesTruncated: truncated[a.ContentVersionID]})
			}
		}
		response.LinkState = "linked"
		response.PersonID = id
		response.NextCursor = page.NextCursor
		response.Truncated = page.Truncated
		return &struct{ Body PersonPhotosResponse }{response}, nil
	})
	put := rawAPIV1Operation("putPersonPhotoLink", http.MethodPut, "/people/by-uid/photo-link", "Link a person to Docbank")
	put.Errors = append(put.Errors, http.StatusNotFound, http.StatusConflict, http.StatusPreconditionFailed, http.StatusServiceUnavailable)
	huma.Register(api, put, func(ctx context.Context, in *struct{ Body PersonPhotoLinkRequest }) (*struct{ Body PersonPhotoLinkResponse }, error) {
		r, err := s.photoReader()
		if err != nil {
			return nil, err
		}
		p, uids, archive, err := photoIdentity(ctx, r, in.Body.UID)
		if err != nil {
			return nil, photoFailure(err)
		}
		uid := p.VCardUID
		if in.Body.State == "unlinked" && in.Body.LinkedUID != "" {
			uid = in.Body.LinkedUID
		}
		if !slices.Contains(uids, uid) {
			return nil, huma.Error400BadRequest("linked UID does not belong to person")
		}
		if in.Body.State == "current" && in.Body.LinkedUID != "" && in.Body.LinkedUID != uid {
			return nil, huma.Error400BadRequest("current link must use canonical UID")
		}
		if in.Body.State == "current" {
			linked, err := photoRemotePerson(ctx, r, archive, uids)
			if err != nil {
				return nil, photoFailure(err)
			}
			if linked != "" && linked != in.Body.PersonID {
				return nil, huma.Error409Conflict("person is linked elsewhere; review required")
			}
			again, currentUIDs, currentArchive, err := photoIdentity(ctx, r, in.Body.UID)
			if err != nil {
				return nil, photoFailure(err)
			}
			if again.ID != p.ID || again.VCardUID != p.VCardUID || again.Revision != p.Revision || currentArchive != archive || !slices.Equal(uids, currentUIDs) {
				return nil, huma.Error409Conflict("person changed; retry")
			}
		}
		identity := docbankmedia.ExternalPersonIdentity{System: "msgvault", ArchiveID: archive, UID: uid, UIDState: in.Body.State, LastSeenRevision: p.Revision}
		if p.DisplayName != nil {
			identity.DisplayNameSnapshot = *p.DisplayName
		}
		_, revision, err := r.Client.SetExternalPersonIdentity(ctx, in.Body.PersonID, in.Body.Revision, identity)
		if err != nil {
			return nil, photoFailure(err)
		}
		return &struct{ Body PersonPhotoLinkResponse }{PersonPhotoLinkResponse{UID: p.VCardUID, LinkedUID: uid, State: in.Body.State, Revision: revision}}, nil
	})
}
