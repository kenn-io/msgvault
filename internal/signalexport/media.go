package signalexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const maxMedia int64 = 256 << 20

// verifiedMedia opens contained regular files and verifies their plaintext hash.
// Long text is retained in bounded memory for the body and archive write.
func verifiedMedia(ctx context.Context, root *os.Root, p *filePointer, limit int64, long bool) (*os.File, []byte, string, error) {
	if p == nil || len(p.Locator.Hash) != 32 {
		return nil, nil, "invalid_hash", nil
	}
	hash := hex.EncodeToString(p.Locator.Hash)
	dir := "files/" + hash[:2]
	d, err := root.Open(dir)
	if err != nil {
		return nil, nil, "missing_or_unsafe", nil
	}
	defer d.Close()
	match := ""
	for {
		entries, err := d.ReadDir(128)
		for _, e := range entries {
			name := e.Name()
			if name != hash && !strings.HasPrefix(name, hash+".") {
				continue
			}
			if match != "" {
				return nil, nil, "ambiguous", nil
			}
			match = name
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, "unreadable_directory", nil
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
	}
	if match == "" {
		return nil, nil, "missing", nil
	}
	rel := dir + "/" + match
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, "unsafe_or_nonregular", nil
	}
	if info.Size() > limit {
		return nil, nil, "oversized", nil
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, "missing_or_unsafe", nil
	}

	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, "changed_file", nil
	}
	h := sha256.New()
	var data []byte
	var n int64
	input := io.LimitReader(&export.ContextReader{Context: ctx, Reader: f}, limit+1)
	if long {
		data, err = io.ReadAll(input)
		n = int64(len(data))
		_, _ = h.Write(data)
	} else {
		n, err = io.Copy(h, input)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, "", ctx.Err()
		}
		return nil, nil, "unreadable", nil
	}
	if n > limit {
		return nil, nil, "oversized", nil
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return nil, nil, "hash_mismatch", nil
	}
	if !long {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return nil, nil, "unreadable", nil
		}
	}
	keep = true
	return f, data, "", nil
}
func mediaWrites(ctx context.Context, st *store.Store, r *reader, messageID int64, m *standardMessage, opts ImportOptions, s *ImportSummary) (string, []store.AttachmentWrite, bool, error) {
	body := ""
	knownChanged := false
	if m.Text != nil {
		body = m.Text.Body
	}
	existing := map[string]store.AttachmentRef{}
	if messageID != 0 {
		refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
		if err != nil {
			return "", nil, false, err
		}
		for _, ref := range refs {
			existing[ref.SourcePartKey] = ref
		}
	}
	type slot struct {
		key  string
		p    *filePointer
		long bool
	}
	slots := make([]slot, 0, len(m.Attachments)+1)
	for i, a := range m.Attachments {
		slots = append(slots, slot{key: fmt.Sprintf("attachment:%d", i), p: a.Pointer})
	}
	if m.LongText != nil {
		slots = append(slots, slot{key: "long_text", p: m.LongText, long: true})
	}
	var writes []store.AttachmentWrite
	for _, slot := range slots {
		prev, found := existing[slot.key]
		if messageID != 0 && (!found || prev.ContentHash != "") && !slot.long {
			continue
		}

		if slot.long && prev.ContentHash != "" && slot.p != nil && hex.EncodeToString(slot.p.Locator.Hash) == prev.ContentHash {
			// Identical archived bytes establish the current long text without the
			// export file. A repaired slot may accompany a retained inline snapshot.
			archived, err := st.GetMessageBodyText(ctx, messageID)
			if err != nil {
				return "", nil, false, err
			}
			digest := sha256.Sum256([]byte(archived))
			knownChanged = hex.EncodeToString(digest[:]) != prev.ContentHash
			body = archived
			continue
		}
		limit := maxMedia
		if slot.long {
			limit = maxRecord
		}
		f, data, reason, err := verifiedMedia(ctx, r.root, slot.p, limit, slot.long)
		if err != nil {
			return "", nil, false, err
		}
		write := store.AttachmentWrite{SourcePartKey: slot.key, Filename: "attachment", MIMEType: "application/octet-stream", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if slot.p != nil {
			if name := filepath.Base(slot.p.FileName); name != "" && name != "." {
				write.Filename = name
			}
			if slot.p.ContentType != "" {
				write.MIMEType = slot.p.ContentType
			}
		}
		if f != nil {
			var input io.Reader = &export.ContextReader{Context: ctx, Reader: f}
			if slot.long {
				body = string(data)
				input = bytes.NewReader(data)
			}
			if messageID != 0 && (!found || prev.ContentHash != "") {
				_ = f.Close()
				continue
			}
			if opts.AttachmentsDir != "" {
				write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, input, limit)
				if errors.Is(err, export.ErrAttachmentTooLarge) {
					reason = "changed_file"
					err = nil
				}
				if err == nil && reason == "" && write.ContentHash != hex.EncodeToString(slot.p.Locator.Hash) {
					reason = "changed_file"
				}
			} else {
				reason = "storage_disabled"
			}
			_ = f.Close()
			if err != nil {
				return "", nil, false, fmt.Errorf("store Signal media: %w", err)
			}
			if reason != "" {
				write.StoragePath, write.ContentHash, write.Size = "", "", 0
			}
		}

		if reason != "" {
			s.AttachmentsMissing++
			s.MediaMissing[reason]++
			if slot.long {
				s.Omitted["incomplete_long_text"]++
			}
			if messageID != 0 {
				continue
			}
		} else {
			s.AttachmentsStored++
		}
		writes = append(writes, write)
	}
	return body, writes, knownChanged, nil
}
