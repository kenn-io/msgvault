package api

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

type photoCursor struct {
	AssertionCursor string `json:"c,omitempty"`
	VersionID       string `json:"v,omitempty"`
	AttachmentID    int64  `json:"a,omitempty"`
}

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
	UID      string `json:"uid" minLength:"1" maxLength:"256"`
	PersonID string `json:"person_id" format:"uuid"`
	Revision int64  `json:"revision" minimum:"1"`
	State    string `json:"state" enum:"current,unlinked"`
}
type PersonPhotoLinkResponse struct {
	UID      string `json:"uid"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

func (s *Server) photoReader() (*DocbankAttachmentReader, error) {
	if s.cfg == nil || !s.cfg.Integrations.Docbank.Enabled || !s.cfg.Integrations.Docbank.AttachmentUploadConsent || s.docbankAttachments == nil || s.docbankAttachments.Store == nil || s.docbankAttachments.Client == nil || s.docbankAttachments.Destination == "" {
		return nil, huma.Error503ServiceUnavailable("Docbank attachment integration unavailable")
	}
	return s.docbankAttachments, nil
}
func photoError(err error) error {
	var remote *docbankmedia.HTTPError
	if status, ok := errors.AsType[huma.StatusError](err); ok {
		return status
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		return huma.Error404NotFound("person not found")
	case errors.Is(err, store.ErrPersonUIDAmbiguous):
		return huma.Error409Conflict("person UID is ambiguous")
	case errors.As(err, &remote) && remote.Status == http.StatusNotFound && remote.Reason == "":
		return huma.Error503ServiceUnavailable("Docbank person lookup unsupported")
	case errors.As(err, &remote) && remote.Status == http.StatusNotFound:
		return huma.Error404NotFound("Docbank person not found")
	case errors.As(err, &remote) && remote.Status == http.StatusUnprocessableEntity:
		return huma.Error422UnprocessableEntity("invalid Docbank request")
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
	archive, err := r.Store.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	return person, uids, archive, nil
}

func photoRemotePerson(ctx context.Context, r *DocbankAttachmentReader, archive string, uids []string) (string, []string, error) {
	var id string
	var resolved []string
	for _, uid := range uids {
		found, err := r.Client.ResolveExternalPerson(ctx, archive, uid)
		if remote, ok := errors.AsType[*docbankmedia.HTTPError](err); ok && remote.Status == http.StatusNotFound && remote.Reason == "not_found" {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if id != "" && id != found.PersonID {
			return "", nil, huma.Error409Conflict("multiple Docbank people linked; review required")
		}
		id = found.PersonID
		if !slices.Contains(resolved, found.ResolvedUID) {
			resolved = append(resolved, found.ResolvedUID)
		}
	}
	return id, resolved, nil
}

func (s *Server) registerPersonPhotoRoutes(api huma.API) {
	get := rawAPIV1Operation("getPersonPhotos", http.MethodGet, "/people/by-uid/photos", "List confirmed person photo attachments")
	get.Errors = append(get.Errors, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	huma.Register(api, get, func(ctx context.Context, in *struct {
		UID    string `query:"uid" required:"true" minLength:"1" maxLength:"256"`
		Cursor string `query:"cursor" maxLength:"8192"`
	}) (*struct{ Body PersonPhotosResponse }, error) {
		position := store.DocumentOccurrencePage{}
		var cursor photoCursor
		if in.Cursor != "" {
			if !strings.HasPrefix(in.Cursor, "1.") {
				return nil, huma.Error400BadRequest("invalid photo cursor")
			}
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(in.Cursor, "1."))
			if err != nil || json.Unmarshal(data, &cursor) != nil || len(cursor.AssertionCursor) > 4096 || (cursor.VersionID == "") != (cursor.AttachmentID == 0) || cursor.AttachmentID < 0 || (cursor.AssertionCursor == "" && cursor.VersionID == "") {
				return nil, huma.Error400BadRequest("invalid photo cursor")
			}
			if cursor.VersionID != "" {
				if _, err := uuid.Parse(cursor.VersionID); err != nil {
					return nil, huma.Error400BadRequest("invalid photo cursor")
				}
			}
			position = store.DocumentOccurrencePage{VersionID: cursor.VersionID, AttachmentID: cursor.AttachmentID}
		}
		r, err := s.photoReader()
		if err != nil {
			return nil, err
		}
		person, uids, archive, err := photoIdentity(ctx, r, in.UID)
		if err != nil {
			return nil, photoError(err)
		}
		id, _, err := photoRemotePerson(ctx, r, archive, uids)
		if err != nil {
			return nil, photoError(err)
		}
		response := PersonPhotosResponse{UID: person.VCardUID, LinkState: "unlinked", Photos: []PersonPhotoEvidence{}}
		if id == "" {
			return &struct{ Body PersonPhotosResponse }{response}, nil
		}
		page, err := r.Client.PersonAssertions(ctx, id, cursor.AssertionCursor)
		if err != nil {
			return nil, photoError(err)
		}
		if page.State != "curated" && page.State != "provisional" {
			return nil, huma.Error409Conflict("Docbank person is not active")
		}
		ids := make([]string, 0, len(page.Assertions))
		for _, a := range page.Assertions {
			if a.Role == "depicted" && a.Action == "assert" {
				ids = append(ids, a.ContentVersionID)
			}
		}
		occurrences, err := r.Store.DocumentSearchOccurrences(ctx, r.Destination, nil, ids, 100, &position)
		if err != nil {
			return nil, photoError(err)
		}
		byVersion := map[string][]MediaAttachmentResult{}
		truncated := map[string]bool{}
		if len(occurrences) > 100 {
			last := occurrences[99]
			truncated[last.VersionID] = occurrences[100].VersionID == last.VersionID
			token, err := json.Marshal(photoCursor{AssertionCursor: cursor.AssertionCursor, VersionID: last.VersionID, AttachmentID: last.AttachmentID})
			if err != nil {
				return nil, photoError(err)
			}
			response.NextCursor = "1." + base64.RawURLEncoding.EncodeToString(token)
			occurrences = occurrences[:100]
		}
		for _, o := range occurrences {
			byVersion[o.VersionID] = append(byVersion[o.VersionID], MediaAttachmentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt})
		}
		for _, a := range page.Assertions {
			if a.Role == "depicted" && a.Action == "assert" && len(byVersion[a.ContentVersionID]) > 0 {
				response.Photos = append(response.Photos, PersonPhotoEvidence{AssertionID: a.AssertionID, ContentVersionID: a.ContentVersionID, Occurrences: byVersion[a.ContentVersionID], OccurrencesTruncated: truncated[a.ContentVersionID]})
			}
		}
		response.LinkState = "linked"
		response.PersonID = id
		if response.NextCursor == "" && page.NextCursor != "" {
			token, err := json.Marshal(photoCursor{AssertionCursor: page.NextCursor})
			if err != nil {
				return nil, photoError(err)
			}
			response.NextCursor = "1." + base64.RawURLEncoding.EncodeToString(token)
		}
		response.Truncated = response.NextCursor != ""
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
			return nil, photoError(err)
		}
		linked, targets, err := photoRemotePerson(ctx, r, archive, uids)
		if err != nil {
			return nil, photoError(err)
		}
		if linked != "" && linked != in.Body.PersonID {
			return nil, huma.Error409Conflict("person is linked elsewhere; review required")
		}
		if in.Body.State == "current" || len(targets) == 0 {
			targets = []string{p.VCardUID}
		}
		revision := in.Body.Revision
		for _, uid := range targets {
			identity := docbankmedia.ExternalPersonIdentity{System: "msgvault", ArchiveID: archive, UID: uid, UIDState: in.Body.State, LastSeenRevision: p.Revision}
			_, revision, err = r.Client.SetExternalPersonIdentity(ctx, in.Body.PersonID, revision, identity)
			if err != nil {
				return nil, photoError(err)
			}
		}
		return &struct{ Body PersonPhotoLinkResponse }{PersonPhotoLinkResponse{UID: p.VCardUID, State: in.Body.State, Revision: revision}}, nil
	})
}
