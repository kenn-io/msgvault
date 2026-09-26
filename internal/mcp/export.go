package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// Chunk bounds. Each result carries its JSON as both text and structured
// content, so a 1 MiB chunk is about 2.8 MB on the wire; gateway sandboxes
// abort on much larger strings.
const (
	defaultChunkBytes = 1 << 20
	maxChunkBytes     = 4 << 20
)

// byteChunk is one verified slice of a larger object. The field names
// match the chunk contract other archive bridges use, so one client can
// download from any of them: fetch from offset 0 until complete, then
// check the concatenation against size and sha256.
type byteChunk struct {
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Complete   bool   `json:"complete"`
	DataBase64 string `json:"data_base64"`
}

type chunkRequest struct {
	offset int64
	length int64
}

// chunkArgs reads the optional offset and length arguments. present
// reports whether the caller supplied either one.
func chunkArgs(args map[string]any) (req chunkRequest, present bool, err error) {
	req.length = defaultChunkBytes
	if raw, ok := args[toolArgOffset]; ok {
		present = true
		v, isNumber := raw.(float64)
		if !isNumber || v != math.Trunc(v) || v < 0 || v > maxJSONSafeInteger {
			return req, present, errors.New("offset must be a non-negative integer")
		}
		req.offset = int64(v)
	}
	if raw, ok := args[toolArgLength]; ok {
		present = true
		v, isNumber := raw.(float64)
		if !isNumber || v != math.Trunc(v) || v < 1 || v > maxChunkBytes {
			return req, present, fmt.Errorf("length must be between 1 and %d", maxChunkBytes)
		}
		req.length = int64(v)
	}
	return req, present, nil
}

func sliceChunk(data []byte, req chunkRequest) (byteChunk, error) {
	size := int64(len(data))
	if req.offset > size {
		return byteChunk{}, fmt.Errorf("offset %d is past the end (size %d)", req.offset, size)
	}
	end := min(req.offset+req.length, size)
	sum := sha256.Sum256(data)
	return byteChunk{
		Offset:     req.offset,
		Length:     end - req.offset,
		Size:       size,
		SHA256:     hex.EncodeToString(sum[:]),
		Complete:   end == size,
		DataBase64: base64.StdEncoding.EncodeToString(data[req.offset:end]),
	}, nil
}

type exportEMLResponse struct {
	MessageID            int64      `json:"message_id"`
	SourceMessageID      string     `json:"source_message_id"`
	ConversationID       int64      `json:"conversation_id"`
	SourceConversationID string     `json:"source_conversation_id"`
	SourceID             int64      `json:"source_id"`
	Account              string     `json:"account"`
	SourceType           string     `json:"source_type"`
	LastSyncAt           *time.Time `json:"last_sync_at,omitzero"`
	Offset               int64      `json:"offset"`
	Length               int64      `json:"length"`
	Size                 int64      `json:"size"`
	SHA256               string     `json:"sha256"`
	Complete             bool       `json:"complete"`
	DataBase64           string     `json:"data_base64"`
}

func (h *handlers) originalMessageReader() (query.OriginalMessageReader, *toolResult) {
	reader, ok := h.engine.(query.OriginalMessageReader)
	if !ok {
		return nil, toolErrorResult(query.ErrOriginalExportUnsupported.Error())
	}
	return reader, nil
}

// messageRefArgs reads exactly one of id or source_message_id, plus account.
func messageRefArgs(args map[string]any) (query.MessageRef, error) {
	var ref query.MessageRef
	ref.Account, _ = args[toolArgAccount].(string)
	ref.SourceMessageID, _ = args[toolArgSourceMsgID].(string)
	if _, ok := args["id"]; ok {
		id, err := getIDArg(args, "id")
		if err != nil {
			return ref, err
		}
		ref.ID = id
	}
	return ref, nil
}

func (h *handlers) exportEML(ctx context.Context, req toolRequest) (*toolResult, error) {
	reader, unsupported := h.originalMessageReader()
	if unsupported != nil {
		return unsupported, nil
	}
	args := req.GetArguments()
	ref, err := messageRefArgs(args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if (ref.ID == 0) == (ref.SourceMessageID == "") {
		return toolErrorResult(query.ErrInvalidMessageRef.Error()), nil
	}
	chunkReq, _, err := chunkArgs(args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	original, err := reader.ReadOriginalMessage(ctx, ref)
	if err != nil {
		return originalExportError("read original message", err)
	}
	chunk, err := sliceChunk(original.MIME, chunkReq)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	record := original.MessageRecord
	return jsonResult(exportEMLResponse{
		MessageID:            record.MessageID,
		SourceMessageID:      record.SourceMessageID,
		ConversationID:       record.ConversationID,
		SourceConversationID: record.SourceConversationID,
		SourceID:             record.SourceID,
		Account:              record.Account,
		SourceType:           record.SourceType,
		LastSyncAt:           record.LastSyncAt,
		Offset:               chunk.Offset,
		Length:               chunk.Length,
		Size:                 chunk.Size,
		SHA256:               chunk.SHA256,
		Complete:             chunk.Complete,
		DataBase64:           chunk.DataBase64,
	})
}

// originalExportError turns the export sentinels into caller-facing tool
// errors and leaves anything else to the shared dependency mapping.
func originalExportError(operation string, err error) (*toolResult, error) {
	switch {
	case errors.Is(err, query.ErrInvalidMessageRef):
		return toolErrorResult(err.Error()), nil
	case errors.Is(err, store.ErrMessageNotFound):
		return toolErrorResult("message not found"), nil
	case errors.Is(err, query.ErrThreadNotFound):
		return toolErrorResult("thread not found"), nil
	case errors.Is(err, query.ErrOriginalMIMEUnavailable):
		return toolErrorResult("raw_mime_unavailable: the archive holds no original MIME for this message " +
			"(chat and calendar sources and some imports store none)"), nil
	case errors.Is(err, query.ErrAmbiguousReference):
		return toolErrorResult("message_ambiguous: " + err.Error() + "; pass account"), nil
	case errors.Is(err, query.ErrOriginalExportUnsupported):
		return toolErrorResult(query.ErrOriginalExportUnsupported.Error()), nil
	}
	return dependencyError(operation, err)
}

func (h *handlers) listThread(ctx context.Context, req toolRequest) (*toolResult, error) {
	reader, unsupported := h.originalMessageReader()
	if unsupported != nil {
		return unsupported, nil
	}
	args := req.GetArguments()
	ref, err := messageRefArgs(args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	q := query.ThreadQuery{MessageRef: ref, Limit: query.ThreadDefaultLimit}
	q.ThreadID, _ = args[toolArgThreadID].(string)
	anchors := 0
	for _, set := range []bool{ref.ID != 0, ref.SourceMessageID != "", q.ThreadID != ""} {
		if set {
			anchors++
		}
	}
	if anchors != 1 {
		return toolErrorResult("provide exactly one of id, source_message_id, or thread_id"), nil
	}
	if raw, ok := args[toolArgLimit]; ok {
		v, isNumber := raw.(float64)
		if !isNumber || v != math.Trunc(v) || v < 1 || v > query.ThreadMaxLimit {
			return toolErrorResult(fmt.Sprintf("limit must be between 1 and %d", query.ThreadMaxLimit)), nil
		}
		q.Limit = int(v)
	}
	if raw, ok := args[toolArgOffset]; ok {
		v, isNumber := raw.(float64)
		if !isNumber || v != math.Trunc(v) || v < 0 || v > math.MaxInt32 {
			return toolErrorResult("offset must be a non-negative integer"), nil
		}
		q.Offset = int(v)
	}
	page, err := reader.ListThread(ctx, q)
	if err != nil {
		return originalExportError("list thread", err)
	}
	return jsonResult(page)
}
