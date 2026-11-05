package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// RepairMessageAttachments restores nonempty MIME attachments from one retained
// Gmail RAW. It never contacts Gmail or rewrites RAW, bodies, labels or existing
// attachment metadata. Corruption and conflicting metadata require inspection.
func (s *Syncer) RepairMessageAttachments(ctx context.Context, request RepairRequest) (repairErr error) {
	if strings.TrimSpace(request.Reference) == "" || request.SourceID < 0 {
		return errors.New("attachment repair requires a message reference and a nonnegative source ID")
	}
	targets, err := s.store.FindRepairMessageTargetsContext(ctx, strings.TrimSpace(request.Reference), request.SourceID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("No matching message") //nolint:staticcheck // ST1005: Preserve the command diagnostic.
	}
	if len(targets) > 1 {
		return errors.New("Multiple messages match; specify --source-id") //nolint:staticcheck // ST1005: Preserve the command diagnostic.
	}
	target := targets[0]
	if target.SourceType != sourceTypeGmail {
		return errors.New("attachment repair requires a Gmail source")
	}
	if s.opts.AttachmentsDir == "" {
		return errors.New("attachment repair requires an attachments directory")
	}
	raw, err := s.store.GetMessageRawContext(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("read retained MIME for attachment repair: %w", err)
	}
	if len(raw) == 0 {
		return errors.New("retained MIME for attachment repair is empty")
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse retained MIME for attachment repair: %w", err)
	}
	// Partial parses cannot certify that every expected occurrence was restored.
	if parsed.Incomplete {
		return errors.New("retained MIME has parse diagnostics; attachment repair is inconclusive")
	}
	parsed.OmitNULIdentities()
	existing, err := s.store.MessageMIMEAttachmentsContext(ctx, target.ID)
	if err != nil {
		return err
	}
	for _, ref := range existing {
		if ref.Size == 0 {
			continue
		}
		matched := false
		for _, att := range parsed.Attachments {
			if (ref.SourcePartKey == "" || ref.SourcePartKey == att.PartKey) &&
				store.AttachmentContentHashesEqual(ref.ContentHash, att.ContentHash) && ref.Size == len(att.Content) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("retained MIME does not match existing attachment %q", ref.Filename)
		}
		if ref.State == "skipped" || ref.State == "unavailable" {
			return fmt.Errorf("attachment %q was deliberately omitted (%s)", ref.Filename, ref.State)
		}
	}
	blobs, err := attachmentstore.New(store.NewPackCatalog(s.store), s.opts.AttachmentsDir)
	if err != nil {
		return err
	}
	defer func() { repairErr = errors.Join(repairErr, blobs.Close()) }()
	created := make(map[string]export.DurableAttachmentReceipt)
	defer func() {
		if repairErr != nil {
			repairErr = errors.Join(repairErr, s.cleanupRepairAttachments(context.WithoutCancel(ctx), created))
		}
	}()
	writes := make([]store.AttachmentWrite, 0, len(parsed.Attachments))
	for _, att := range parsed.Attachments {
		if len(att.Content) == 0 {
			continue
		} // Ordinary sync deliberately omits empty content.
		if err := ctx.Err(); err != nil {
			return err
		}
		reader, size, readErr := blobs.OpenStream(ctx, att.ContentHash)
		if readErr == nil {
			_, copyErr := io.Copy(io.Discard, reader)
			readErr = errors.Join(copyErr, reader.Close())
			if readErr == nil && size != int64(len(att.Content)) {
				readErr = errors.New("stored attachment size differs from retained MIME")
			}
		}
		storagePath := att.ContentHash[:2] + "/" + att.ContentHash
		if readErr != nil {
			// Preserve damaged or unavailable evidence. Only a physically missing
			// blob (or a new hash with no membership yet) may be materialized.
			if errors.Is(readErr, packstore.ErrPhysicalCorrupt) ||
				(!errors.Is(readErr, fs.ErrNotExist) && !errors.Is(readErr, packstore.ErrPhysicalMissing) && !errors.Is(readErr, packstore.ErrPhysicalAuthorityMissing)) {
				return fmt.Errorf("verify attachment %q: %w", att.Filename, readErr)
			}
			location, err := store.NewPackCatalog(s.store).Resolve(ctx, packstore.Hash(att.ContentHash))
			if err != nil {
				return err
			}
			if location.Pack != nil {
				return fmt.Errorf("attachment %q requires repair of its missing pack: %w", att.Filename, readErr)
			}
			receipt, err := export.StoreAttachmentFileDurable(s.opts.AttachmentsDir, &att)
			if receipt.Created {
				created[receipt.StoragePath] = receipt
			}
			if err != nil {
				return fmt.Errorf("restore attachment %q: %w", att.Filename, err)
			}
			storagePath = receipt.StoragePath
		}
		role, roleSource := store.AttachmentRoleFromMIME(att.Disposition, att.IsInline, att.ContentID)
		writes = append(writes, store.AttachmentWrite{
			Filename: textutil.EnsureUTF8(att.Filename), MIMEType: textutil.EnsureUTF8(att.ContentType),
			StoragePath: storagePath, ContentHash: att.ContentHash, Size: int64(len(att.Content)),
			Role: role, RoleSource: roleSource, SourcePartKey: att.PartKey, ContentID: att.ContentID,
		})
	}
	return s.store.RestoreMIMEAttachmentRecordsContext(ctx, target.MessageIdentityGuard, writes)
}
