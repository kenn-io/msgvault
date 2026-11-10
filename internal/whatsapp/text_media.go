package whatsapp

import (
	"context"
	"errors"
	"io/fs"
	"mime"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var textAttached = regexp.MustCompile(`^<\s*attached:\s*(.+?)\s*>$`)

func importTextMedia(ctx context.Context, st *store.Store, root *os.Root, messageID int64, body string, opts TextOptions, summary *TextSummary) error {
	for line := range strings.SplitSeq(body, "\n") {
		marker := strings.TrimSpace(strings.TrimLeft(line, "\u200e\u200f"))
		if strings.EqualFold(marker, "<Media omitted>") || strings.HasSuffix(strings.TrimSpace(line), "\u200eimage omitted") {
			summary.MediaOmitted++
			continue
		}
		ref := ""
		if match := textAttached.FindStringSubmatch(marker); match != nil {
			ref = strings.TrimSpace(match[1])
		} else if name, ok := strings.CutSuffix(marker, " (file attached)"); ok {
			ref = name
		}
		if ref == "" {
			if strings.HasPrefix(marker, "<") && (strings.Contains(marker, "attached") || strings.Contains(marker, "omitted")) {
				summary.MediaSkipped++
			}
			continue
		}
		// Export paths are portable relative names; deny Windows drives/backslashes
		// as well as POSIX traversal, before opening anything under the supplied root.
		unsafe := !fs.ValidPath(ref) || strings.ContainsAny(ref, "\\:\x00")
		if unsafe {
			summary.MediaSkipped++
			continue
		}
		if root == nil {
			summary.MediaMissing++
			continue
		}
		info, err := root.Stat(ref)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				summary.MediaMissing++
			} else {
				summary.MediaSkipped++
			}
			continue
		}
		if !info.Mode().IsRegular() {
			summary.MediaSkipped++
			continue
		}
		const limit = 100 << 20
		if info.Size() > limit {
			summary.MediaOversized++
			continue
		}
		f, err := root.Open(ref)
		if err != nil {
			summary.MediaSkipped++
			continue
		}
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			summary.MediaSkipped++
			continue
		}
		storage, hash, size, err := export.StoreAttachmentStream(ctx, opts.AttachmentsDir, f, limit)
		closeErr := f.Close()
		if errors.Is(err, export.ErrAttachmentTooLarge) {
			summary.MediaOversized++
			continue
		}
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		mimeType := mime.TypeByExtension(path.Ext(ref))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		want := store.AttachmentRef{
			Filename: path.Base(ref), MimeType: mimeType, StoragePath: storage, ContentHash: hash, Size: int(size),
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "whatsapp_txt:" + textHash(ref),
		}
		existing, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
		if err != nil {
			return err
		}
		if !slices.Contains(existing, want) {
			if err := st.UpsertAttachmentRecordWithStats(ctx, messageID, store.AttachmentWrite{
				Filename: want.Filename, MIMEType: want.MimeType, StoragePath: want.StoragePath, ContentHash: want.ContentHash, Size: size,
				Role: want.Role, RoleSource: want.RoleSource, SourcePartKey: want.SourcePartKey,
			}, true); err != nil {
				return err
			}
		}
		summary.MediaStored++
	}
	return nil
}
