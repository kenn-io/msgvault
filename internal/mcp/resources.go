package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
)

const attachmentResourceTemplate = "msgvault://attachment/{id}"

type attachmentPayload struct {
	metadata *query.AttachmentInfo
	mimeType string
	data     []byte
}

type attachmentUnavailableError struct {
	message string
}

func (e *attachmentUnavailableError) Error() string { return e.message }

type attachmentService struct {
	engine         query.Engine
	attachmentsDir string
	reader         AttachmentReader
}

func (h *handlers) attachmentService() attachmentService {
	return attachmentService{
		engine:         h.engine,
		attachmentsDir: h.attachmentsDir,
		reader:         h.attachmentReader,
	}
}

func attachmentResourceURI(id int64) string {
	return "msgvault://attachment/" + strconv.FormatInt(id, 10)
}

func parseAttachmentResourceURI(rawURI string) (int64, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return 0, fmt.Errorf("parse attachment resource URI: %w", err)
	}
	if parsed.Scheme != "msgvault" || parsed.Host != "attachment" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return 0, errors.New("invalid attachment resource URI")
	}
	segment := strings.TrimPrefix(parsed.Path, "/")
	if segment == "" || strings.Contains(segment, "/") {
		return 0, errors.New("invalid attachment resource ID")
	}
	id, err := strconv.ParseInt(segment, 10, 64)
	if err != nil || id < 1 || id > int64(maxJSONSafeInteger) {
		return 0, errors.New("invalid attachment resource ID")
	}
	if rawURI != attachmentResourceURI(id) {
		return 0, errors.New("non-canonical attachment resource URI")
	}
	return id, nil
}

func (s attachmentService) load(ctx context.Context, id int64) (*attachmentPayload, error) {
	return s.loadBounded(ctx, id, maxAttachmentSize)
}

func (s attachmentService) loadBounded(ctx context.Context, id, maxBytes int64) (*attachmentPayload, error) {
	attachment, err := s.engine.GetAttachment(ctx, id)
	if err != nil {
		return nil, newInternalError("look up attachment", err)
	}
	if attachment == nil {
		return nil, &attachmentUnavailableError{message: "attachment not found"}
	}
	if s.reader == nil && s.attachmentsDir == "" {
		return nil, &attachmentUnavailableError{message: "attachments directory not configured"}
	}
	if attachment.Size > maxBytes {
		return nil, &attachmentUnavailableError{message: fmt.Sprintf(
			"attachment too large: %d bytes (max %d)", attachment.Size, maxBytes,
		)}
	}

	data, err := s.read(ctx, attachment.ContentHash, maxBytes)
	if err != nil {
		return nil, err
	}
	mimeType := attachment.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return &attachmentPayload{metadata: attachment, mimeType: mimeType, data: data}, nil
}

func (s attachmentService) read(ctx context.Context, contentHash string, maxBytes int64) ([]byte, error) {
	if err := export.ValidateContentHash(contentHash); err != nil {
		return nil, &attachmentUnavailableError{message: "attachment has invalid content hash"}
	}
	if s.reader != nil {
		data, err := s.reader.ReadAttachment(ctx, contentHash)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, &attachmentUnavailableError{message: "attachment file not available"}
			}
			return nil, newInternalError("read attachment", err)
		}
		if int64(len(data)) > maxBytes {
			return nil, &attachmentUnavailableError{message: fmt.Sprintf(
				"attachment too large: %d bytes (max %d)", len(data), maxBytes,
			)}
		}
		return data, nil
	}

	filePath, err := export.StoragePath(s.attachmentsDir, contentHash)
	if err != nil {
		return nil, &attachmentUnavailableError{message: "attachment has invalid content hash"}
	}
	file, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &attachmentUnavailableError{message: "attachment file not available"}
		}
		return nil, newInternalError("open attachment", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, newInternalError("stat attachment", err)
	}
	if info.Size() > maxBytes {
		return nil, &attachmentUnavailableError{message: fmt.Sprintf(
			"attachment too large: %d bytes (max %d)", info.Size(), maxBytes,
		)}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, newInternalError("read attachment", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, &attachmentUnavailableError{message: fmt.Sprintf(
			"attachment too large: %d bytes (max %d)", len(data), maxBytes,
		)}
	}
	return data, nil
}

func registerAttachmentResources(server *sdkmcp.Server, h *handlers) {
	server.AddResourceTemplate(&sdkmcp.ResourceTemplate{
		Name:        "archive_attachment",
		Title:       "Archived attachment",
		Description: "Read one archived attachment by its opaque numeric ID.",
		URITemplate: attachmentResourceTemplate,
	}, func(ctx context.Context, req *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		rawURI := req.Params.URI
		id, err := parseAttachmentResourceURI(rawURI)
		if err != nil {
			return nil, sdkmcp.ResourceNotFoundError(rawURI)
		}
		payload, err := h.attachmentService().load(ctx, id)
		if err != nil {
			if _, ok := errors.AsType[*attachmentUnavailableError](err); ok {
				return nil, sdkmcp.ResourceNotFoundError(rawURI)
			}
			return nil, mapInternalError(err)
		}
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{
			{
				URI:      rawURI,
				MIMEType: payload.mimeType,
				Blob:     payload.data,
			},
		}}, nil
	})
}
