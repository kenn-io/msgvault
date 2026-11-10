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

func mediaWrites(ctx context.Context, st *store.Store, r *exportReader, messageID int64, filename string, m *Message, opts ImportOptions, s *ImportSummary) ([]store.AttachmentWrite, error) {
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
	var writes []store.AttachmentWrite
	for i, ref := range m.Media {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slot := fmt.Sprintf("media:%d", i)
		if previous, ok := existing[slot]; ok && previous.ContentHash != "" {
			continue
		}
		name, kind := mediaPath(filename, ref)
		w := store.AttachmentWrite{SourcePartKey: slot, Filename: path.Base(name), MediaType: "document", MIMEType: mime.TypeByExtension(strings.ToLower(path.Ext(name))), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		metadata, err := json.Marshal(struct {
			Reference string `json:"reference"`
		}{ref})
		if err != nil {
			return nil, err
		}
		w.Metadata = string(metadata)
		if w.Filename == "." || w.Filename == "" {
			w.Filename = slot
		}
		if w.MIMEType == "" {
			w.MIMEType = "application/octet-stream"
		}
		switch strings.SplitN(w.MIMEType, "/", 2)[0] {
		case "image":
			w.MediaType = "image"
		case "video":
			w.MediaType = "video"
		case "audio":
			w.MediaType = "audio"
		}
		switch kind {
		case "remote":
			s.MediaRemote++
		case "invalid":
			s.MediaInvalid++
		case "local":
			f, err := r.fsys.Open(name)
			if errors.Is(err, fs.ErrNotExist) {
				s.MediaMissing++
				break
			}
			if err != nil {
				return nil, fmt.Errorf("tumblr: %s: open media %q (%s): %w", filename, ref, slot, err)
			}
			info, statErr := f.Stat()
			if statErr != nil {
				_ = f.Close()
				return nil, fmt.Errorf("tumblr: %s: inspect media %q (%s): %w", filename, ref, slot, statErr)
			}
			if !info.Mode().IsRegular() {
				s.MediaInvalid++
				_ = f.Close()
				break
			}
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
				return nil, fmt.Errorf("tumblr: %s: store media %q (%s): %w", filename, ref, slot, err)
			}
			s.MediaStored++
		}
		if messageID == 0 || w.ContentHash != "" {
			writes = append(writes, w)
		}
	}
	return writes, nil
}
