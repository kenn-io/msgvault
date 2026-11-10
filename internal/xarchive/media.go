package xarchive

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// mediaPath follows the archive viewer: message ID plus the URL filename.
// Local paths must stay inside the matching media folder.
func mediaPath(value, messageID string, group bool) (string, bool) {
	dir := "direct_messages_media"
	if group {
		dir = "direct_messages_group_media"
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	if !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	filename := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `\`) {
		return "", false
	}
	return dir + "/" + messageID + "-" + filename, true
}

func mediaWrites(ctx context.Context, st *store.Store, root *os.Root, messageID int64, m *message, group bool, dest string, summary *ImportSummary) ([]store.AttachmentWrite, error) {
	if len(m.Media) == 0 {
		return nil, nil
	}
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
	writes := []store.AttachmentWrite{}
	for i, original := range m.Media {
		key := "media:" + strconv.Itoa(i)
		if messageID != 0 {
			old, ok := existing[key]
			if !ok || old.ContentHash != "" {
				continue
			}
		}
		relative, ok := mediaPath(original, m.ID, group)
		meta, err := json.Marshal(map[string]string{"original_url": original, "source_file": relative})
		if err != nil {
			return nil, err
		}
		write := store.AttachmentWrite{SourcePartKey: key, Filename: path.Base(relative), Metadata: string(meta), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if write.Filename == "." || write.Filename == "" {
			write.Filename = "media"
		}
		write.MIMEType = mime.TypeByExtension(path.Ext(write.Filename))
		if write.MIMEType == "" {
			write.MIMEType = "application/octet-stream"
		}
		if ok && dest != "" {
			// Stat before open avoids blocking on FIFOs and rejects other nonregular sources.
			info, err := root.Stat(relative)
			if err == nil && info.Mode().IsRegular() && info.Size() <= attachmentpolicy.DefaultChatMaxBytes {
				f, openErr := root.Open(relative)
				if openErr == nil {
					write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, dest, f, attachmentpolicy.DefaultChatMaxBytes)
					closeErr := f.Close()
					if errors.Is(err, export.ErrAttachmentTooLarge) {
						err = nil
					} else if err != nil {
						return nil, fmt.Errorf("xarchive: store media: %w", err)
					}
					if closeErr != nil {
						return nil, closeErr
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
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
