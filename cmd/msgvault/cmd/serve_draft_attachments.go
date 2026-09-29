package cmd

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func matchForwardAttachmentRef(part msgmime.Attachment, refs []store.AttachmentRef, used []bool) int {
	if part.PartKey != "" {
		for i, ref := range refs {
			if used[i] || ref.SourcePartKey == "" || ref.SourcePartKey != part.PartKey {
				continue
			}
			return i
		}
	}
	for i, ref := range refs {
		if used[i] || ref.Filename != part.Filename || !strings.EqualFold(ref.ContentHash, part.ContentHash) {
			continue
		}
		if ref.ContentID != "" && ref.ContentID != part.ContentID {
			continue
		}
		return i
	}
	return -1
}

// prepareIMAPDraftAttachmentWrites binds generated MIME occurrences to the
// catalog references that already own their bytes. A hash by itself never
// creates a new catalog reference.
func prepareIMAPDraftAttachmentWrites(ctx context.Context, parsed *msgmime.Message, refs []store.AttachmentRef) ([]store.AttachmentWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, fmt.Errorf("parse generated draft before preparing attachments")
	}
	writes := make([]store.AttachmentWrite, 0, len(parsed.Attachments))
	used := make([]bool, len(refs))
	for _, part := range parsed.Attachments {
		refIndex := matchForwardAttachmentRef(part, refs, used)
		if refIndex < 0 || refs[refIndex].ContentHash == "" {
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
