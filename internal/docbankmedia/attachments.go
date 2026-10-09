package docbankmedia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// AttachmentNode is the stable identity returned by Docbank's file routes.
type AttachmentNode struct {
	ID        int64  `json:"id"`
	ParentID  int64  `json:"parent_id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
	VersionID string `json:"current_version_id"`
}

type AttachmentReceipt struct {
	Status       string         `json:"status"`
	Node         AttachmentNode `json:"node"`
	ComputedHash string         `json:"computed_hash"`
	ComputedSize int64          `json:"computed_size"`
}

// EnsureCollection resolves or creates each directory in the virtual path.
func (c *Client) EnsureCollection(ctx context.Context, collection string) (int64, error) {
	if !strings.HasPrefix(collection, "/") || path.Clean(collection) != collection {
		return 0, ErrInvalidRequest
	}
	var node AttachmentNode
	var current strings.Builder
	for segment := range strings.SplitSeq(strings.TrimPrefix(collection, "/"), "/") {
		if segment == "" {
			continue
		}
		_, _ = current.WriteString("/" + segment)
		currentPath := current.String()
		endpoint := "/api/v1/path?" + url.Values{"path": {currentPath}}.Encode()
		err := c.jsonRequest(ctx, http.MethodGet, endpoint, nil, &node)
		if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusNotFound {
			err = c.jsonRequest(ctx, http.MethodPost, "/api/v1/path/mkdir", struct {
				Path string `json:"path"`
			}{currentPath}, &node)
			if conflict, ok := errors.AsType[*HTTPError](err); ok && conflict.Status == http.StatusConflict {
				err = c.jsonRequest(ctx, http.MethodGet, endpoint, nil, &node)
			}
		}
		if err != nil {
			return 0, err
		}
		if node.ID <= 0 || node.Kind != "dir" {
			return 0, ErrInvalidReceipt
		}
	}
	if current.Len() == 0 {
		if err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/path?path=%2F", nil, &node); err != nil {
			return 0, err
		}
	}
	if node.ID <= 0 || node.Kind != "dir" {
		return 0, ErrInvalidReceipt
	}
	return node.ID, nil
}

// UploadAttachment uses exactly one file part; this API accepts no source metadata.
// The immutable name is saved locally before upload so interrupted retries converge.
func (c *Client) UploadAttachment(ctx context.Context, parent int64, name, mediaType, hash string, size int64, content io.Reader) (AttachmentReceipt, error) {
	digest, err := hex.DecodeString(hash)
	if err != nil || len(digest) != 32 || strings.ToLower(hash) != hash || size < 0 || size > maxMediaBytes || parent <= 0 || name == "" || strings.ContainsAny(name, "/\\\r\n") || content == nil {
		return AttachmentReceipt{}, ErrInvalidRequest
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return AttachmentReceipt{}, ErrInvalidRequest
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", multipart.FileContentDisposition("file", name))
		header.Set("Content-Type", mediaType)
		part, err := multipartWriter.CreatePart(header)
		if err == nil {
			var n int64
			n, err = io.Copy(part, io.LimitReader(content, size+1))
			if err == nil && n != size {
				err = ErrInvalidRequest
			}
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	defer func() { _ = reader.Close(); <-done }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/uploads?"+url.Values{"parent_id": {strconv.FormatInt(parent, 10)}, "name": {name}}.Encode(), reader)
	if err != nil {
		return AttachmentReceipt{}, err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("X-Docbank-Blob-Hash", hash)
	req.Header.Set("X-Docbank-Blob-Size", strconv.FormatInt(size, 10))
	if err := c.setAPIKey(req); err != nil {
		return AttachmentReceipt{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return AttachmentReceipt{}, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readResponse(resp, maxResponseBytes)
	if err != nil {
		return AttachmentReceipt{}, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return AttachmentReceipt{}, &HTTPError{Status: resp.StatusCode, Code: statusCode(resp.StatusCode)}
	}
	var receipt AttachmentReceipt
	err = json.Unmarshal(data, &receipt)
	if err != nil || receipt.ComputedHash != hash || receipt.ComputedSize != size || receipt.Node.ID <= 0 || receipt.Node.ParentID != parent || receipt.Node.Name != name || receipt.Node.Kind != "file" || receipt.Node.BlobHash != hash || receipt.Node.Size != size || receipt.Node.VersionID == "" || (receipt.Status != "added" && receipt.Status != "skipped") {
		return AttachmentReceipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

// AttachmentDestinationKey namespaces receipts by archive, daemon, and collection.
func AttachmentDestinationKey(endpoint, archiveUID, collection string) string {
	digest := sha256.Sum256([]byte("docbank-attachments/v1\x00" + strings.TrimRight(strings.TrimSpace(endpoint), "/") + "\x00" + archiveUID + "\x00" + collection))
	return "attachments:" + hex.EncodeToString(digest[:])
}
