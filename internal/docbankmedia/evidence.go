package docbankmedia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrEvidenceUnavailable means Docbank no longer holds the delivered file.
var ErrEvidenceUnavailable = errors.New("docbank evidence unavailable")

// EvidenceWindowRequest mirrors Docbank's POST /api/v1/evidence/windows body.
type EvidenceWindowRequest struct {
	VaultUID              string `json:"vault_uid"`
	NodeID                int64  `json:"node_id"`
	ContentVersionID      string `json:"content_version_id"`
	ContentSHA256         string `json:"content_sha256"`
	RenditionAttachmentID string `json:"rendition_attachment_id"`
	BuildID               string `json:"build_id"`
	RenditionSHA256       string `json:"rendition_sha256"`
	Offset                int    `json:"offset,omitzero"`
	MaxChars              int    `json:"max_chars,omitzero"`
}

// EvidenceWindow mirrors Docbank's evidence window response.
type EvidenceWindow struct {
	VaultUID              string `json:"vault_uid"`
	NodeID                int64  `json:"node_id"`
	ContentVersionID      string `json:"content_version_id"`
	ContentSHA256         string `json:"content_sha256"`
	RenditionAttachmentID string `json:"rendition_attachment_id"`
	BuildID               string `json:"build_id"`
	RenditionSHA256       string `json:"rendition_sha256"`
	Text                  string `json:"text"`
	ActualStart           int    `json:"actual_start"`
	ActualEnd             int    `json:"actual_end"`
	NextOffset            int    `json:"next_offset"`
	EOF                   bool   `json:"eof"`
	ResponseBytes         int    `json:"response_bytes"`
	MediaType             string `json:"media_type"`
}

// DestinationKey names one Docbank endpoint for one archive, the key the
// media job records each delivery under. endpoint is the configured URL.
func DestinationKey(endpoint, archiveUID string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	digest := sha256.Sum256([]byte("beeper-media/v1\x00" + endpoint + "\x00" + archiveUID))
	return "beeper:" + hex.EncodeToString(digest[:])
}

// EvidenceIdentity returns the node and selected rendition identities of one
// content version, without VaultUID or a range. It reads only the rendition's
// headers, so it never starts processing. When Docbank selects no rendition it
// returns the 404 with the node ID set, so the caller can ask the node why. A
// version Docbank no longer holds returns ErrEvidenceUnavailable.
func (c *Client) EvidenceIdentity(ctx context.Context, contentVersionID, contentSHA256, profile string) (EvidenceWindowRequest, error) {
	var version struct {
		ID       string `json:"id"`
		NodeID   int64  `json:"node_id"`
		BlobHash string `json:"blob_hash"`
	}
	err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/versions/"+url.PathEscape(contentVersionID), nil, &version)
	// Docbank pruned the version or emptied its trash.
	if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusNotFound {
		return EvidenceWindowRequest{}, ErrEvidenceUnavailable
	}
	if err != nil {
		return EvidenceWindowRequest{}, err
	}
	if version.ID != contentVersionID || version.NodeID < 1 || !strings.EqualFold(version.BlobHash, contentSHA256) {
		return EvidenceWindowRequest{}, ErrEvidenceUnavailable
	}
	type selector struct {
		NodeID           int64  `json:"node_id"`
		ContentVersionID string `json:"content_version_id"`
		Profile          string `json:"profile"`
	}
	body := struct {
		Selector selector `json:"selector"`
		MaxBytes int64    `json:"max_bytes"`
	}{selector{version.NodeID, version.ID, profile}, 64 << 20}
	header, err := c.headers(ctx, http.MethodPost, "/api/v1/renditions/select", body)
	if err != nil {
		return EvidenceWindowRequest{NodeID: version.NodeID}, err
	}
	identity := EvidenceWindowRequest{NodeID: version.NodeID, ContentVersionID: version.ID, ContentSHA256: version.BlobHash,
		RenditionAttachmentID: header.Get("X-Docbank-Rendition-Attachment"), BuildID: header.Get("X-Docbank-Rendition-Build"), RenditionSHA256: header.Get("X-Docbank-Blob-Hash")}
	if header.Get("X-Docbank-Content-Version") != version.ID || identity.RenditionAttachmentID == "" || identity.BuildID == "" || identity.RenditionSHA256 == "" {
		return EvidenceWindowRequest{}, ErrInvalidReceipt
	}
	return identity, nil
}

// CurrentVersion returns the content version a Docbank node holds now. A
// deleted or trashed node returns ErrEvidenceUnavailable.
func (c *Client) CurrentVersion(ctx context.Context, nodeID int64) (string, error) {
	var node struct {
		ID               int64  `json:"id"`
		CurrentVersionID string `json:"current_version_id"`
		TrashedAt        string `json:"trashed_at"`
	}
	err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/nodes/"+strconv.FormatInt(nodeID, 10), nil, &node)
	if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusNotFound {
		return "", ErrEvidenceUnavailable
	}
	if err != nil {
		return "", err
	}
	if node.ID != nodeID || node.TrashedAt != "" || node.CurrentVersionID == "" {
		return "", ErrEvidenceUnavailable
	}
	return node.CurrentVersionID, nil
}

// ReadEvidenceWindow reads one window of a pinned rendition. Docbank checks
// the identities; the window must start at the requested offset, since callers
// slice the text by it.
func (c *Client) ReadEvidenceWindow(ctx context.Context, request EvidenceWindowRequest) (EvidenceWindow, error) {
	var w EvidenceWindow
	if err := c.jsonRequest(ctx, http.MethodPost, "/api/v1/evidence/windows", request, &w); err != nil {
		return EvidenceWindow{}, err
	}
	if w.ActualStart != request.Offset {
		return EvidenceWindow{}, ErrInvalidReceipt
	}
	return w, nil
}

// headers sends a JSON request and returns the response headers, closing a
// successful body unread.
func (c *Client) headers(ctx context.Context, method, endpoint string, body any) (http.Header, error) {
	req, err := c.newJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
		return nil, newHTTPError(resp.StatusCode, data)
	}
	return resp.Header, nil
}
