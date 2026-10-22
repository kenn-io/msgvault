package docbankmedia

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// AttachmentNode is the stable identity returned by Docbank's file routes.
type AttachmentNode struct {
	ID        int64  `json:"id"`
	ParentID  int64  `json:"parent_id"`
	Kind      string `json:"kind"`
	BlobHash  string `json:"blob_hash"`
	VersionID string `json:"current_version_id"`
}

type AttachmentReceipt struct {
	Status       string         `json:"status"`
	Node         AttachmentNode `json:"node"`
	ComputedHash string         `json:"computed_hash"`
	ComputedSize int64          `json:"computed_size"`
}

// EnsureFolder resolves or creates each directory in the virtual path.
func (c *Client) EnsureFolder(ctx context.Context, folder string) (int64, error) {
	var node AttachmentNode
	var current strings.Builder
	var err error
	for segment := range strings.SplitSeq(strings.TrimPrefix(folder, "/"), "/") {
		if segment == "" {
			continue
		}
		current.WriteString("/" + segment)
		if node, err = c.ensureDirectory(ctx, current.String()); err != nil {
			return 0, err
		}
	}
	if current.Len() == 0 {
		if err = c.jsonRequest(ctx, http.MethodGet, "/api/v1/path?path=%2F", nil, &node); err != nil {
			return 0, err
		}
	}
	if node.ID <= 0 || node.Kind != "dir" {
		return 0, ErrInvalidReceipt
	}
	return node.ID, nil
}

// ensureDirectory decodes every response into a fresh node so a field one response omits cannot keep an earlier value.
func (c *Client) ensureDirectory(ctx context.Context, path string) (AttachmentNode, error) {
	endpoint := "/api/v1/path?" + url.Values{"path": {path}}.Encode()
	var node AttachmentNode
	err := c.jsonRequest(ctx, http.MethodGet, endpoint, nil, &node)
	if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusNotFound {
		err = c.jsonRequest(ctx, http.MethodPost, "/api/v1/path/mkdir", struct {
			Path string `json:"path"`
		}{path}, &node)
		if conflict, ok := errors.AsType[*HTTPError](err); ok && conflict.Status == http.StatusConflict {
			err = c.jsonRequest(ctx, http.MethodGet, endpoint, nil, &node)
		}
	}
	return node, err
}

// UploadAttachment uses exactly one file part; this API accepts no source metadata.
// The caller supplies a sanitized name, media type and positive parent.
func (c *Client) UploadAttachment(ctx context.Context, parent int64, name, mediaType, hash string, size int64, content io.Reader) (AttachmentReceipt, error) {
	endpoint := "/api/v1/uploads?" + url.Values{"parent_id": {strconv.FormatInt(parent, 10)}, "name": {name}}.Encode()
	data, err := c.multipart(ctx, endpoint, http.Header{
		"X-Docbank-Blob-Hash": {hash},
		"X-Docbank-Blob-Size": {strconv.FormatInt(size, 10)},
	}, func(writer *multipart.Writer) error {
		part, err := filePart(writer, name, mediaType)
		if err != nil {
			return err
		}
		_, err = io.Copy(part, content)
		if err != nil {
			return err
		}
		return writer.Close()
	})
	if err != nil {
		if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusRequestEntityTooLarge {
			httpErr.Code = "content_too_large"
		}
		return AttachmentReceipt{}, err
	}
	var receipt AttachmentReceipt
	err = json.Unmarshal(data, &receipt)
	if err != nil || receipt.ComputedHash != hash || receipt.ComputedSize != size || receipt.Node.ID <= 0 || receipt.Node.ParentID != parent || receipt.Node.Kind != "file" || receipt.Node.BlobHash != hash || receipt.Node.VersionID == "" || (receipt.Status != "added" && receipt.Status != "skipped") {
		return AttachmentReceipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

// VaultUID identifies the vault selected by the configured Docbank endpoint.
func (c *Client) VaultUID(ctx context.Context) (string, error) {
	if cached := c.vaultUID.Load(); cached != nil {
		return *cached, nil
	}
	var info struct {
		VaultID string `json:"vault_id"`
	}
	if err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/info", nil, &info); err != nil {
		return "", err
	}
	if info.VaultID == "" {
		return "", ErrInvalidReceipt
	}
	c.vaultUID.Store(&info.VaultID)
	return info.VaultID, nil
}

func (c *Client) clearVaultUID() {
	c.vaultUID.Store(nil)
}
