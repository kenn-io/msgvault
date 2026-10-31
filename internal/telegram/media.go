package telegram

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

func isMediaWarning(value string) bool {
	switch value {
	case "(File unavailable, please try again later)",
		"(File exceeds maximum size. Change data exporting settings to download.)",
		"(File not included. Change data exporting settings to download.)",
		"(Photo unavailable, please try again later)",
		"(Photo exceeds maximum size. Change data exporting settings to download.)",
		"(Photo not included. Change data exporting settings to download.)":
		return true
	}
	return false
}

// mediaWrites prepares fixed slots before the message transaction. Existing
// slots with bytes are left intact, even when a later export renames files.
func mediaWrites(ctx context.Context, st *store.Store, messageID int64, m *Message, opts ImportOptions, summary *ImportSummary) ([]store.AttachmentWrite, error) {
	existing := map[string]store.AttachmentRef{}
	if messageID != 0 {
		refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			existing[ref.SourcePartKey] = ref
		}
	}
	var writes []store.AttachmentWrite
	for _, slot := range []struct{ key, path string }{{"photo", m.Photo}, {"file", m.File}, {"thumbnail", m.Thumbnail}} {
		if slot.path == "" {
			continue
		}
		if messageID != 0 {
			previous, ok := existing[slot.key]
			if !ok || previous.ContentHash != "" {
				continue
			}
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
		if isMediaWarning(slot.path) {
			summary.AttachmentsMissing++
			if messageID == 0 {
				write.Filename = slot.key
				writes = append(writes, write)
			}
			continue
		}
		if !filepath.IsLocal(slot.path) {
			return nil, fmt.Errorf("telegram: media path %q must be local to the export", slot.path)
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
			var err error
			write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentFromPath(opts.AttachmentsDir, filepath.Join(opts.RootDir, slot.path), 0)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("telegram: store media %q: %w", slot.path, err)
			}
			if err != nil {
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
