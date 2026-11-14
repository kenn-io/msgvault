package signalexport

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const maxMedia int64 = 256 << 20

// verifiedMedia opens contained regular files; long text is bounded and
// verified in memory. Ordinary media is verified while streaming to storage.
func verifiedMedia(ctx context.Context, r *reader, p *filePointer, limit int64, long bool) (io.ReadCloser, []byte, string, error) {
	if p == nil || len(p.Locator.Hash) != 32 {
		return nil, nil, "invalid_hash", nil
	}
	hash := hex.EncodeToString(p.Locator.Hash)
	name, reason, err := r.mediaName(ctx, hash)
	if err != nil || reason != "" {
		return nil, nil, reason, err
	}
	rel := "files/" + hash[:2] + "/" + name
	info, err := r.root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, "unsafe_or_nonregular", nil //nolint:nilerr // Unavailable export media is counted and retried instead of failing the import.
	}
	if info.Size() > limit {
		return nil, nil, "oversized", nil
	}
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, nil, "missing_or_unsafe", nil //nolint:nilerr // Unavailable export media is counted and retried instead of failing the import.
	}
	if !long {
		src, err := contentverify.NewReadCloser(f, hash)
		if err != nil {
			_ = f.Close()
			return nil, nil, "invalid_hash", nil //nolint:nilerr // Invalid export media is counted and retried instead of failing the import.
		}
		return src, nil, "", nil
	}
	data, err := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: f}, limit+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, nil, "", fmt.Errorf("read Signal media %s: %w", rel, err)
	}
	if int64(len(data)) > limit {
		return nil, nil, "oversized", nil
	}
	if err := contentverify.VerifyBytes(data, hash); err != nil {
		return nil, nil, "hash_mismatch", nil //nolint:nilerr // Corrupt export media is counted and retried instead of failing the import.
	}
	return io.NopCloser(bytes.NewReader(data)), data, "", nil
}

func mediaWrites(ctx context.Context, st *store.Store, r *reader, messageID int64, m *standardMessage, opts ImportOptions, s *ImportSummary) (string, []store.AttachmentWrite, bool, error) {
	body := ""
	knownChanged := false
	if m.Text != nil {
		body = m.Text.Body
	}
	mediaSlots, err := export.LoadMediaSlots(ctx, st, messageID)
	if err != nil {
		return "", nil, false, err
	}
	existing := mediaSlots.Existing
	needRaw := false
	for _, ref := range existing {
		needRaw = needRaw || ref.ContentHash == ""
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

	archived := map[string]*filePointer{}
	if needRaw {
		raw, err := st.GetMessageRawContext(ctx, messageID)
		if err != nil {
			return "", nil, false, fmt.Errorf("read saved attachment details: %w", err)
		}
		var old chatItem
		if err := json.Unmarshal(raw, &old); err != nil {
			return "", nil, false, fmt.Errorf("read saved attachment details: %w", err)
		}
		if old.Standard != nil {
			slots = nil
			claimed := map[int]bool{}
			for i, a := range old.Standard.Attachments {
				key := fmt.Sprintf("attachment:%d", i)
				p := a.Pointer
				match := matchingPointer(a, old.Standard.Attachments, m, claimed)
				if p == nil || len(p.Locator.Hash) == 0 {
					p = match
				}
				archived[key] = p
				slots = append(slots, slot{key: key, p: p})
			}
			if old.Standard.LongText != nil {
				p := old.Standard.LongText
				if m.LongText == nil || len(p.Locator.Key) == 0 || !bytes.Equal(p.Locator.Key, m.LongText.Locator.Key) {
					p = nil
				} else if len(p.Locator.Hash) == 0 {
					p = m.LongText
				}
				archived["long_text"] = p
				slots = append(slots, slot{key: "long_text", p: m.LongText, long: true})
			}
		}
	}
	var writes []store.AttachmentWrite
	for _, slot := range slots {
		prev, found := existing[slot.key]
		if !mediaSlots.Pending(slot.key) && !slot.long {
			continue
		}

		if slot.long && prev.ContentHash != "" && slot.p != nil && hex.EncodeToString(slot.p.Locator.Hash) == prev.ContentHash {
			// Identical archived bytes establish the current long text without the
			// export file. A repaired slot may accompany a retained inline snapshot.
			archived, err := st.GetMessageBodyText(ctx, messageID)
			if err != nil {
				return "", nil, false, err
			}
			knownChanged = contentverify.VerifyBytes([]byte(archived), prev.ContentHash) != nil
			body = archived
			continue
		}
		limit := maxMedia
		if slot.long {
			limit = maxRecord
		}
		p := slot.p
		repair := messageID != 0 && found && prev.ContentHash == ""
		if repair {
			p = archived[slot.key]
		}
		src, data, reason, err := verifiedMedia(ctx, r, p, limit, slot.long)
		if err != nil {
			return "", nil, false, err
		}
		write := store.AttachmentWrite{SourcePartKey: slot.key, Filename: "attachment", MIMEType: "application/octet-stream", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if p != nil {
			if name := filepath.Base(p.FileName); name != "" && name != "." {
				write.Filename = name
			}
			if p.ContentType != "" {
				write.MIMEType = p.ContentType
			}
		}
		if src != nil {
			if slot.long && (!repair || (slot.p != nil && bytes.Equal(p.Locator.Hash, slot.p.Locator.Hash))) {
				body = string(data)
			}
			if messageID != 0 && (!found || prev.ContentHash != "") {
				_ = src.Close()
				continue
			}
			write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, src, limit)
			err = errors.Join(err, src.Close())
			switch {
			case ctx.Err() != nil:
				return "", nil, false, ctx.Err()
			case errors.Is(err, contentverify.ErrMismatch):
				reason = "hash_mismatch"
			case errors.Is(err, export.ErrAttachmentTooLarge):
				reason = "changed_file"
			case err != nil:
				return "", nil, false, fmt.Errorf("read or store Signal media %s: %w", hex.EncodeToString(p.Locator.Hash), err)
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
			if !mediaSlots.Keep(write) {
				continue
			}
		} else {
			s.AttachmentsStored++
		}
		writes = append(writes, write)
	}
	return body, writes, knownChanged, nil
}

// matchingPointer accepts only a unique, unclaimed identity in the new export.
func matchingPointer(old messageAttachment, originals []messageAttachment, m *standardMessage, claimed map[int]bool) *filePointer {
	for _, identity := range []func(messageAttachment) []byte{
		func(a messageAttachment) []byte { return a.ClientUUID },
		func(a messageAttachment) []byte {
			if a.Pointer == nil {
				return nil
			}
			return a.Pointer.Locator.Key
		},
	} {
		value := identity(old)
		if len(value) == 0 {
			continue
		}
		originalCount := 0
		for _, attachment := range originals {
			if bytes.Equal(value, identity(attachment)) {
				originalCount++
			}
		}
		if originalCount != 1 {
			continue
		}
		match, count := -1, 0
		for i, attachment := range m.Attachments {
			if attachment.Pointer != nil && bytes.Equal(value, identity(attachment)) {
				match, count = i, count+1
			}
		}
		if count == 1 && !claimed[match] {
			claimed[match] = true
			return m.Attachments[match].Pointer
		}
	}
	return nil
}
