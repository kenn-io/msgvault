package docbankmedia

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type ExternalPersonResolution struct {
	PersonID     string `json:"person_id"`
	System       string `json:"system"`
	ArchiveID    string `json:"archive_id"`
	RequestedUID string `json:"requested_uid"`
	ResolvedUID  string `json:"resolved_uid"`
	UIDState     string `json:"uid_state"`
	Forwarded    bool   `json:"forwarded"`
}

type ExternalPersonIdentity struct {
	System              string `json:"system"`
	ArchiveID           string `json:"archive_id"`
	UID                 string `json:"uid"`
	UIDState            string `json:"uid_state"`
	UIDKind             string `json:"uid_kind,omitempty"`
	DisplayNameSnapshot string `json:"display_name_snapshot,omitempty"`
	LastSeenRevision    int64  `json:"last_seen_revision,omitempty"`
}

type PersonAssertion struct {
	AssertionID      string `json:"assertion_id"`
	ContentVersionID string `json:"content_version_id"`
	PersonID         string `json:"person_id"`
	Role             string `json:"role"`
	Action           string `json:"action"`
}

type PersonAssertions struct {
	PersonID   string            `json:"person_id"`
	State      string            `json:"state"`
	Revision   int64             `json:"revision"`
	Assertions []PersonAssertion `json:"assertions"`
	NextCursor string            `json:"assertions_next_cursor"`
	Truncated  bool              `json:"assertions_truncated"`
}

func (c *Client) ResolveExternalPerson(ctx context.Context, archiveID, uid string) (ExternalPersonResolution, error) {
	var out ExternalPersonResolution
	if archiveID == "" || uid == "" {
		return out, ErrInvalidRequest
	}
	query := url.Values{"system": {"msgvault"}, "archive_id": {archiveID}, "uid": {uid}}
	if err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/people/resolve-external?"+query.Encode(), nil, &out); err != nil {
		return out, err
	}
	if out.PersonID == "" || out.System != "msgvault" || out.ArchiveID != archiveID || out.RequestedUID != uid || out.ResolvedUID == "" || out.UIDState != "current" || out.Forwarded != (out.ResolvedUID != uid) {
		return ExternalPersonResolution{}, ErrInvalidReceipt
	}
	return out, nil
}

func (c *Client) SetExternalPersonIdentity(ctx context.Context, personID string, revision int64, identity ExternalPersonIdentity) (ExternalPersonIdentity, int64, error) {
	var out ExternalPersonIdentity
	req, err := c.newJSONRequest(ctx, http.MethodPut, "/api/v1/people/by-id/"+url.PathEscape(personID)+"/external-identities", identity, maxMetadataBytes)
	if err != nil {
		return out, 0, err
	}
	req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(revision, 10)))
	resp, err := c.http.Do(req)
	if err != nil {
		return out, 0, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readResponse(resp, maxResponseBytes)
	if err != nil {
		return out, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, 0, newHTTPError(resp.StatusCode, data)
	}
	tag, err := strconv.Unquote(resp.Header.Get("ETag"))
	if err != nil {
		return out, 0, ErrInvalidReceipt
	}
	newRevision, err := strconv.ParseInt(tag, 10, 64)
	if err != nil || newRevision <= revision {
		return out, 0, ErrInvalidReceipt
	}
	if json.Unmarshal(data, &out) != nil || out.System != identity.System || out.ArchiveID != identity.ArchiveID || out.UID != identity.UID || out.UIDState != identity.UIDState || out.UIDKind != "vcard_uid" {
		return ExternalPersonIdentity{}, 0, ErrInvalidReceipt
	}
	return out, newRevision, nil
}

func (c *Client) PersonAssertions(ctx context.Context, personID, cursor string) (PersonAssertions, error) {
	var out PersonAssertions
	query := url.Values{"assertion_limit": {"100"}}
	if cursor != "" {
		query.Set("assertion_cursor", cursor)
	}
	if err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/people/by-id/"+url.PathEscape(personID)+"?"+query.Encode(), nil, &out); err != nil {
		return out, err
	}
	if out.PersonID != personID || out.Revision < 1 || out.State == "" || out.Assertions == nil || len(out.Assertions) > 100 || len(out.NextCursor) > 4096 || out.Truncated != (out.NextCursor != "") || (out.NextCursor != "" && out.NextCursor == cursor) {
		return PersonAssertions{}, ErrInvalidReceipt
	}
	for _, a := range out.Assertions {
		if a.PersonID != personID || a.AssertionID == "" || a.ContentVersionID == "" || a.Role == "" || a.Action == "" {
			return PersonAssertions{}, fmt.Errorf("%w: invalid assertion", ErrInvalidReceipt)
		}
	}
	return out, nil
}
