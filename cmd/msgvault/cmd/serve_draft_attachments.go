package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// forwardDraftAttachmentWrites carries a generated forward's catalog rows onto
// its replacement message. It returns nil for other drafts, which carry no
// attachments.
func (a *storeAPIAdapter) forwardDraftAttachmentWrites(ctx context.Context, currentMessageID int64, replacement imaplib.ReplyDraft) ([]store.AttachmentWrite, error) {
	if !imaplib.IsGeneratedForward(replacement.Raw) {
		return nil, nil
	}
	refs, err := a.store.MessageMIMEAttachmentsContext(ctx, currentMessageID)
	if err != nil {
		return nil, err
	}
	return prepareIMAPDraftAttachmentWrites(ctx, replacement.Parsed, refs)
}

// matchForwardAttachmentRef prefers the first unused keyed row with the part's
// metadata, then a keyless legacy row. Part keys are not compared: a forward
// numbers its parts differently from the message its rows came from.
func matchForwardAttachmentRef(part msgmime.Attachment, refs []store.AttachmentRef, used []bool) int {
	named, legacy := -1, -1
	for i, ref := range refs {
		if !strings.EqualFold(ref.ContentHash, part.ContentHash) {
			continue
		}
		if ref.SourcePartKey != "" {
			// Keyed rows record one occurrence each, with its own Content-ID.
			if !used[i] && ref.Filename == part.Filename && ref.ContentID == part.ContentID {
				return i
			}
			continue
		}
		// Keyless legacy rows are unique per hash, keep only the first part's
		// metadata and may lack a Content-ID, so one may back several identical parts.
		if named < 0 && ref.Filename == part.Filename && (ref.ContentID == "" || ref.ContentID == part.ContentID) {
			named = i
		}
		if legacy < 0 {
			legacy = i
		}
	}
	if named >= 0 {
		return named
	}
	return legacy
}

// prepareIMAPDraftAttachmentWrites binds generated MIME occurrences to the
// catalog references that already own their bytes. A hash by itself never
// creates a new catalog reference.
func prepareIMAPDraftAttachmentWrites(ctx context.Context, parsed *msgmime.Message, refs []store.AttachmentRef) ([]store.AttachmentWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, errors.New("parse generated draft before preparing attachments")
	}
	writes := make([]store.AttachmentWrite, 0, len(parsed.Attachments))
	used := make([]bool, len(refs))
	for _, part := range parsed.Attachments {
		if part.Size == 0 {
			continue // sync stores no row for an empty part
		}
		refIndex := matchForwardAttachmentRef(part, refs, used)
		if refIndex < 0 {
			return nil, fmt.Errorf("generated attachment %q (%s) has no retained catalog reference", part.Filename, part.PartKey)
		}
		ref := refs[refIndex]
		used[refIndex] = true
		if ref.State != "" && ref.State != attachmentpolicy.StateStored {
			return nil, fmt.Errorf("attachment %q is %s", ref.Filename, ref.State)
		}
		role, roleSource := store.AttachmentRoleFromMIME(part.Disposition, part.IsInline, part.ContentID)
		writes = append(writes, store.AttachmentWrite{
			Filename:      part.Filename,
			MIMEType:      part.ContentType,
			StoragePath:   ref.StoragePath,
			ContentHash:   strings.ToLower(ref.ContentHash),
			Size:          int64(part.Size),
			MediaType:     ref.MediaType,
			Width:         ref.Width,
			Height:        ref.Height,
			DurationMS:    ref.DurationMS,
			Metadata:      ref.Metadata,
			Role:          role,
			RoleSource:    roleSource,
			SourcePartKey: part.PartKey,
			ContentID:     part.ContentID,
			State:         ref.State,
			SkipReason:    ref.SkipReason,
		})
	}
	return writes, nil
}
