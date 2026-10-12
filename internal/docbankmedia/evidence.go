package docbankmedia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var (
	// ErrEvidenceUnavailable means Docbank no longer holds the delivered file.
	ErrEvidenceUnavailable = errors.New("docbank evidence unavailable")
	// ErrNoRendition means Docbank selects no rendition of the version: it is
	// no longer the node's current version, or it hasn't been processed.
	ErrNoRendition = errors.New("docbank selects no rendition of the version")
	// ErrRenditionFailed means Docbank's rendition work for the version failed.
	ErrRenditionFailed = errors.New("docbank rendition failed")
	// ErrProfileUnavailable means Docbank has no usable processing profile.
	ErrProfileUnavailable = errors.New("docbank processing profile unavailable")
)

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
// content version, without VaultUID or a range. It resolves metadata without
// streaming rendition bytes. When Docbank selects no rendition it returns
// ErrNoRendition, or Docbank's 404, with the node ID set, so the caller can
// ask the node why. A version Docbank no longer holds returns
// ErrEvidenceUnavailable.
func (c *Client) EvidenceIdentity(ctx context.Context, contentVersionID, contentSHA256, profile string) (EvidenceWindowRequest, error) {
	var version struct {
		ID       string `json:"id"`
		NodeID   int64  `json:"node_id"`
		BlobHash string `json:"blob_hash"`
		Size     int64  `json:"size"`
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
	var node struct {
		ID               int64  `json:"id"`
		Revision         int64  `json:"revision"`
		CurrentVersionID string `json:"current_version_id"`
		TrashedAt        string `json:"trashed_at"`
	}
	err = c.jsonRequest(ctx, http.MethodGet, "/api/v1/nodes/"+strconv.FormatInt(version.NodeID, 10), nil, &node)
	if err != nil {
		return EvidenceWindowRequest{NodeID: version.NodeID}, err
	}
	if node.ID != version.NodeID || node.Revision < 1 {
		return EvidenceWindowRequest{}, ErrInvalidReceipt
	}
	if node.TrashedAt != "" {
		return EvidenceWindowRequest{}, ErrEvidenceUnavailable
	}
	if node.CurrentVersionID != version.ID {
		return EvidenceWindowRequest{NodeID: version.NodeID}, ErrNoRendition
	}
	body := struct {
		NodeID    int64  `json:"node_id"`
		Revision  int64  `json:"revision"`
		VersionID string `json:"version_id"`
		BlobHash  string `json:"blob_hash"`
		Size      int64  `json:"size"`
		Profile   string `json:"profile"`
	}{node.ID, node.Revision, version.ID, version.BlobHash, version.Size, profile}
	var receipt struct {
		State  string `json:"state"`
		Source struct {
			NodeID    int64  `json:"node_id"`
			Revision  int64  `json:"revision"`
			VersionID string `json:"version_id"`
			BlobHash  string `json:"blob_hash"`
			Size      int64  `json:"size"`
		} `json:"source"`
		Profile struct {
			Name string `json:"name"`
		} `json:"profile"`
		AttachmentID string `json:"attachment_id"`
		BuildID      string `json:"build_id"`
		Artifact     *struct {
			SHA256 string `json:"sha256"`
		} `json:"artifact"`
	}
	if err := c.jsonRequest(ctx, http.MethodPost, "/api/v1/renditions/text", body, &receipt); err != nil {
		return EvidenceWindowRequest{NodeID: node.ID}, err
	}
	if receipt.State == "unconfigured" || receipt.State == "profile_required" {
		return EvidenceWindowRequest{NodeID: node.ID}, ErrProfileUnavailable
	}
	if receipt.State == "failed" {
		return EvidenceWindowRequest{NodeID: node.ID}, ErrRenditionFailed
	}
	if receipt.State == "historical_unavailable" || receipt.State == "verified_empty" {
		return EvidenceWindowRequest{NodeID: node.ID}, ErrEvidenceUnavailable
	}
	if receipt.State == "unprocessed" {
		return EvidenceWindowRequest{NodeID: node.ID}, ErrNoRendition
	}
	if receipt.State != "ready" {
		return EvidenceWindowRequest{NodeID: node.ID}, ErrInvalidReceipt
	}
	identity := EvidenceWindowRequest{NodeID: version.NodeID, ContentVersionID: version.ID, ContentSHA256: version.BlobHash,
		RenditionAttachmentID: receipt.AttachmentID, BuildID: receipt.BuildID}
	if receipt.Artifact != nil {
		identity.RenditionSHA256 = receipt.Artifact.SHA256
	}
	if receipt.Source.NodeID != node.ID || receipt.Source.Revision != node.Revision || receipt.Source.VersionID != version.ID ||
		receipt.Source.BlobHash != version.BlobHash || receipt.Source.Size != version.Size || receipt.Profile.Name != profile ||
		identity.RenditionAttachmentID == "" || identity.BuildID == "" || identity.RenditionSHA256 == "" {
		return EvidenceWindowRequest{}, ErrInvalidReceipt
	}
	return identity, nil
}

// SuppliedTranscriptMatches verifies the producer's retained input identity.
func SuppliedTranscriptMatches(retained, selected string) bool {
	return retained != "" && selected == retained
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
