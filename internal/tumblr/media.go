package tumblr

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/url"
	"path"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

func mediaPath(filename, ref string) (string, string) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", "invalid"
	}
	if u.Scheme != "" || u.Host != "" || strings.HasPrefix(ref, "//") {
		if (u.Scheme == "" || strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Opaque == "" && strings.TrimSpace(u.Hostname()) != "" {
			return "", "remote"
		}
		return "", "invalid"
	}
	if u.Scheme != "" || u.Path == "" || strings.HasPrefix(u.Path, "/") || strings.ContainsAny(u.Path, `\:`) {
		return "", "invalid"
	}
	// url.Parse decodes escapes once. Clean after joining, allowing sibling media.
	name := path.Join(path.Dir(filename), u.Path)
	if !fs.ValidPath(name) {
		return "", "invalid"
	}
	return name, "local"
}

func mediaWrites(ctx context.Context, st *store.Store, r *exportReader, messageID int64, filename string, m *Message, opts ImportOptions, s *ImportSummary) (writes []store.AttachmentWrite, prepared int, retErr error) {
	existing := map[string]store.AttachmentRef{}
	if messageID != 0 && len(m.Media) > 0 {
		refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
		if err != nil {
			return nil, 0, err
		}
		for _, ref := range refs {
			existing[ref.SourcePartKey] = ref
		}
	}
	for i, ref := range m.Media {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		slot := fmt.Sprintf("media:%d", i)
		if previous, ok := existing[slot]; ok && previous.ContentHash != "" {
			continue
		}
		name, kind := mediaPath(filename, ref)
		w := store.AttachmentWrite{SourcePartKey: slot, Filename: path.Base(name), MediaType: "document", MIMEType: mime.TypeByExtension(strings.ToLower(path.Ext(name))), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		metadata, err := json.Marshal(map[string]string{"reference": ref})
		if err != nil {
			return nil, 0, err
		}
		w.Metadata = string(metadata)
		if w.Filename == "." || w.Filename == "" {
			w.Filename = slot
		}
		if w.MIMEType == "" {
			w.MIMEType = "application/octet-stream"
		}
		switch mediaType, _, _ := strings.Cut(w.MIMEType, "/"); mediaType {
		case "image", "video", "audio":
			w.MediaType = mediaType
		}
		switch kind {
		case "remote":
			s.MediaRemote++
		case "invalid":
			s.MediaInvalid++
		case "local":
			f, err := r.openRegular(name)
			switch {
			case err == nil:
				if opts.AttachmentsDir == "" {
					s.MediaMissing++
					_ = f.Close()
					break
				}
				w.StoragePath, w.ContentHash, w.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, &export.ContextReader{Context: ctx, Reader: f}, maxMediaBytes)
				err = errors.Join(err, f.Close())
				if errors.Is(err, export.ErrAttachmentTooLarge) {
					s.MediaOversized++
					break
				}
				if err != nil {
					return nil, 0, fmt.Errorf("tumblr: %s: store media %q (%s): %w", filename, ref, slot, err)
				}
				prepared++
			case errors.Is(err, fs.ErrNotExist):
				s.MediaMissing++
			case errors.Is(err, errNonregular):
				s.MediaInvalid++
			default:
				return nil, 0, fmt.Errorf("tumblr: %s: open media %q (%s): %w", filename, ref, slot, err)
			}
		}
		if messageID == 0 || w.ContentHash != "" {
			writes = append(writes, w)
		}
	}
	return writes, prepared, nil
}
