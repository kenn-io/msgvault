package groupme

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// galleryName implements only the image/video naming observed in the pinned
// export viewer. No URL is fetched and no loose gallery file is guessed.
func galleryName(id string, a Attachment) (string, bool) {
	u, err := url.Parse(a.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	var parts []string
	switch a.Type {
	case "image":
		parts = strings.Split(strings.TrimPrefix(u.Path, "/"), ".")
		if len(parts) != 3 {
			return "", false
		}
		parts = []string{parts[0], parts[2], parts[1]}
	case "video":
		parts = strings.Split(filepath.Base(u.Path), ".")
		if len(parts) != 3 {
			return "", false
		}
	default:
		return "", false
	}
	// Every component must be a single filename component, including opaque IDs.
	for _, part := range append([]string{id}, parts...) {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return "", false
		}
	}
	name := id + "_" + strings.Join(parts, ".")
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return "", false
	}
	return name, true
}

func mediaWrites(ctx context.Context, st *store.Store, root *os.Root, folder string, messageID int64, m *Message, opts ImportOptions, summary *ImportSummary) ([]store.AttachmentWrite, error) {
	var existing map[string]store.AttachmentRef
	var writes []store.AttachmentWrite
	for _, a := range m.Attachments {
		if a.Type == "reply" {
			continue
		}
		name, supported := galleryName(m.ID, a)
		if !supported {
			summary.AttachmentsUnsupported++
			continue
		}
		key := a.Type + ":" + name
		if messageID != 0 && existing == nil {
			refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
			if err != nil {
				return nil, err
			}
			existing = map[string]store.AttachmentRef{}
			for _, ref := range refs {
				existing[ref.SourcePartKey] = ref
			}
		}
		if prior, ok := existing[key]; messageID != 0 && (!ok || prior.ContentHash != "") {
			continue
		}
		metadata, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		write := store.AttachmentWrite{Metadata: string(metadata), SourcePartKey: key, Filename: name, MediaType: a.Type, MIMEType: mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if write.MIMEType == "" {
			write.MIMEType = "application/octet-stream"
		}
		oversized := false
		if opts.AttachmentsDir != "" {
			err := statRegular(root, filepath.Join(folder, "gallery", name))
			var file *os.File
			if err == nil {
				file, err = root.Open(filepath.Join(folder, "gallery", name))
			}
			if err == nil {
				info, statErr := file.Stat()
				if statErr != nil {
					_ = file.Close()
					return nil, statErr
				}
				oversized = info.Size() > attachmentpolicy.DefaultChatMaxBytes
				if !info.Mode().IsRegular() || oversized {
					_ = file.Close()
				} else {
					write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, file, attachmentpolicy.DefaultChatMaxBytes)
					oversized = errors.Is(err, export.ErrAttachmentTooLarge)
					err = errors.Join(err, file.Close())
					if err != nil && !errors.Is(err, export.ErrAttachmentTooLarge) {
						return nil, fmt.Errorf("groupme: store media: %w", err)
					}
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Missing, inaccessible, escaped and oversized files keep retryable empty slots.
		}
		if write.ContentHash == "" {
			if oversized {
				summary.AttachmentsOversized++
			} else {
				summary.AttachmentsMissing++
			}
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
