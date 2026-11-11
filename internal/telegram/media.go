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

// mediaWrites prepares fixed slots before the message transaction. Existing
// slots with bytes are left intact, even when a later export renames files.
func mediaWrites(ctx context.Context, st *store.Store, messageID int64, m *Message, opts ImportOptions, summary *ImportSummary) ([]store.AttachmentWrite, error) {
	if m.Photo == "" && m.File == "" && m.Thumbnail == "" && m.ContactVCard == "" {
		return nil, nil
	}
	var missing map[string]bool
	if messageID != 0 {
		var err error
		missing, err = st.MissingMIMEAttachmentSlotsContext(ctx, messageID)
		if err != nil {
			return nil, err
		}
	}
	var writes []store.AttachmentWrite
	for _, slot := range []struct{ key, path string }{{"photo", m.Photo}, {"file", m.File}, {"thumbnail", m.Thumbnail}, {"contact_vcard", m.ContactVCard}} {
		if slot.path == "" {
			continue
		}
		if messageID != 0 && !missing[slot.key] {
			continue
		}
		write := store.AttachmentWrite{SourcePartKey: slot.key, MediaType: m.MediaType, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if write.MediaType == "" {
			write.MediaType = slot.key
		}
		if slot.key == "thumbnail" {
			write.MediaType, write.Role, write.RoleSource = "thumbnail", store.AttachmentRoleThumbnail, store.AttachmentRoleSourceProviderExplicit
		} else if m.MediaType == "sticker" {
			write.Role, write.RoleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
		}
		invalidPath := !filepath.IsLocal(slot.path)
		if invalidPath && opts.Logger != nil {
			opts.Logger.Warn("telegram: media path must be local to the export; recording missing media", "slot", slot.key, "path", slot.path)
		}
		if isMediaWarning(slot.path) || invalidPath {
			summary.AttachmentsMissing++
			if messageID == 0 {
				write.Filename = slot.key
				writes = append(writes, write)
			}
			continue
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
		if opts.AttachmentsDir != "" {
			source := filepath.Join(opts.RootDir, slot.path)
			file, err := export.OpenAttachmentSource(source, 0)
			if err == nil {
				write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, file, 0)
				err = errors.Join(err, file.Close())
			}
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
		}
		if write.ContentHash == "" {
			summary.AttachmentsMissing++
			if messageID != 0 {
				continue
			}
		} else {
			summary.AttachmentsStored++
		}
		writes = append(writes, write)
	}
	return writes, nil
}
