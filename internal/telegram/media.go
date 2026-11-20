package telegram

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

func isMediaWarning(value string) bool {
	switch value {
	case "(File unavailable, please try again later)",
		"(File exceeds maximum size. Change data exporting settings to download.)",
		"(File not included. Change data exporting settings to download.)":
		return true
	}
	return false
}

type mediaSlot struct {
	key  string
	path string
}

func mediaSlots(m *Message) []mediaSlot {
	return []mediaSlot{
		{key: "photo", path: m.Photo},
		{key: "file", path: m.File},
		{key: "thumbnail", path: m.Thumbnail},
		{key: "contact_vcard", path: m.ContactVCard},
	}
}

func attachmentWrite(m *Message, slot mediaSlot) store.AttachmentWrite {
	write := store.AttachmentWrite{SourcePartKey: slot.key, MediaType: m.MediaType, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
	if write.MediaType == "" {
		write.MediaType = slot.key
	}
	if slot.key == "thumbnail" {
		write.MediaType, write.Role, write.RoleSource = "thumbnail", store.AttachmentRoleThumbnail, store.AttachmentRoleSourceProviderExplicit
	} else if m.MediaType == "sticker" {
		write.Role, write.RoleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
	}
	write.Filename = filepath.Base(slot.path)
	if slot.key == "file" {
		if m.FileName != "" {
			write.Filename = m.FileName
		}
		write.MIMEType = m.MimeType
	}
	if write.MIMEType == "" {
		write.MIMEType = mime.TypeByExtension(strings.ToLower(filepath.Ext(write.Filename)))
	}
	if write.MIMEType == "" {
		write.MIMEType = "application/octet-stream"
	}
	return write
}

// mediaPlaceholders gives each new message a retryable media slot without
// copying files before the message batch commits.
func mediaPlaceholders(m *Message) []store.AttachmentWrite {
	if !m.hasMedia() {
		return nil
	}
	var writes []store.AttachmentWrite
	for _, slot := range mediaSlots(m) {
		if slot.path == "" {
			continue
		}
		write := attachmentWrite(m, slot)
		if isMediaWarning(slot.path) || !filepath.IsLocal(slot.path) {
			write.Filename, write.MIMEType = slot.key, ""
		}
		writes = append(writes, write)
	}
	return writes
}

func storeMedia(ctx context.Context, slot mediaSlot, write *store.AttachmentWrite, opts ImportOptions) error {
	if opts.AttachmentsDir == "" {
		return nil
	}
	source := filepath.Join(opts.RootDir, slot.path)
	file, err := export.OpenAttachmentSource(source, 0)
	if err != nil {
		return err
	}
	write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, file, 0)
	return errors.Join(err, file.Close())
}

// mediaWrites fills missing attachment slots after the message is saved.
// Existing slots with bytes stay intact, even when a later export renames files.
func mediaWrites(ctx context.Context, st *store.Store, messageID int64, m *Message, opts ImportOptions, summary *ImportSummary) ([]store.AttachmentWrite, error) {
	if !m.hasMedia() {
		return nil, nil
	}
	existing := map[string]store.AttachmentRef{}
	refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		existing[ref.SourcePartKey] = ref
	}
	var writes []store.AttachmentWrite
	for _, slot := range mediaSlots(m) {
		if slot.path == "" {
			continue
		}
		previous, ok := existing[slot.key]
		if !ok || previous.ContentHash != "" {
			continue
		}
		write := attachmentWrite(m, slot)
		invalidPath := !filepath.IsLocal(slot.path)
		if invalidPath && opts.Logger != nil {
			opts.Logger.Warn("telegram: media path must be local to the export; recording missing media", "slot", slot.key, "path", slot.path)
		}
		if isMediaWarning(slot.path) || invalidPath {
			summary.AttachmentsMissing++
			continue
		}
		source := filepath.Join(opts.RootDir, slot.path)
		err := storeMedia(ctx, slot, &write, opts)
		pathErr, sourceError := errors.AsType[*os.PathError](err)
		if err != nil && (!sourceError || pathErr.Path != source) {
			return nil, fmt.Errorf("telegram: store media %q: %w", slot.path, err)
		}
		if err != nil {
			if opts.Logger != nil {
				opts.Logger.Warn("telegram: cannot open media source; recording missing media", "slot", slot.key, "path", source, "error", err)
			}
			write.StoragePath, write.ContentHash, write.Size = "", "", 0
		}
		if write.ContentHash == "" {
			summary.AttachmentsMissing++
			continue
		}
		summary.AttachmentsStored++
		writes = append(writes, write)
	}
	return writes, nil
}
