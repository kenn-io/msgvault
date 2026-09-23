package imazingcsv

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const defaultMaxAttachmentBytes int64 = 100 << 20

type attachmentEntry struct {
	path      string
	regular   bool
	directory bool
}

type attachmentIndex struct {
	exact    map[string]attachmentEntry
	basename map[string][]attachmentEntry
}

func newAttachmentIndex(root string) (*attachmentIndex, error) {
	index := &attachmentIndex{
		exact: make(map[string]attachmentEntry), basename: make(map[string][]attachmentEntry),
	}
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return index, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect iMazing attachments directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("iMazing attachments path %q is not a real directory", root)
	}
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		item := attachmentEntry{
			path: filePath, regular: entry.Type().IsRegular(), directory: entry.IsDir(),
		}
		index.exact[key] = item
		base := path.Base(key)
		index.basename[base] = append(index.basename[base], item)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index iMazing attachments: %w", err)
	}
	return index, nil
}

func (index *attachmentIndex) resolve(reference string) (string, bool, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(reference, `\`, "/"))
	pathQualified := strings.Contains(normalized, "/")
	if strings.IndexByte(normalized, 0) >= 0 || normalized == "" || path.IsAbs(normalized) {
		return "", false, fmt.Errorf("unsafe iMazing attachment reference %q", reference)
	}
	first := normalized
	if slash := strings.IndexByte(first, '/'); slash >= 0 {
		first = first[:slash]
	}
	if len(first) >= 2 && first[1] == ':' {
		return "", false, fmt.Errorf("unsafe iMazing attachment reference %q", reference)
	}
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("unsafe iMazing attachment reference %q", reference)
	}
	prefix := ""
	parts := strings.Split(clean, "/")
	for _, part := range parts[:len(parts)-1] {
		prefix = path.Join(prefix, part)
		if item, ok := index.exact[prefix]; ok && !item.directory {
			return "", false, fmt.Errorf("unsafe iMazing attachment reference %q", reference)
		}
	}
	if item, ok := index.exact[clean]; ok {
		if !item.regular {
			return "", false, fmt.Errorf("iMazing attachment reference %q is not a regular file", reference)
		}
		return item.path, true, nil
	}
	// Path-qualified references must match exactly: a basename fallback
	// could archive unrelated content from another directory
	// (nested/photo.jpg must never resolve to other/photo.jpg), so a
	// qualified miss is reported as missing.
	if pathQualified {
		return "", false, nil
	}
	matches := index.basename[clean]
	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		if !matches[0].regular {
			return "", false, fmt.Errorf("iMazing attachment reference %q is not a regular file", reference)
		}
		return matches[0].path, true, nil
	default:
		// No unique source file: retain a missing occurrence for a later export.
		return "", false, nil
	}
}

func importAttachments(
	ctx context.Context,
	st *store.Store,
	layout Layout,
	opts Options,
	plans []*plannedMessage,
) (stored, missing, skipped int, retErr error) {
	index, err := newAttachmentIndex(layout.AttachmentsDir)
	if err != nil {
		return 0, 0, 0, err
	}
	maxBytes := opts.MaxAttachmentBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxAttachmentBytes
	}
	// Attachment rows are independent and their messages are already
	// persisted with attachment claims, so one row's resolve, store, or
	// persist failure must not drop later messages' occurrences: every
	// failed row records a typed failed occurrence, later valid rows still
	// store, and the aggregate error is returned once all rows were handled.
	var rowErrs []error
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return stored, missing, skipped, errors.Join(append(rowErrs, err)...)
		}
		reference := strings.TrimSpace(plan.row.Attachment)
		keepSourcePartKey := ""
		var write store.AttachmentWrite
		if reference != "" {
			write, err = attachmentWrite(plan.row, reference)
			if err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("%s record %d: %w", plan.row.File, plan.row.Record, err))
				continue
			}
			keepSourcePartKey = write.SourcePartKey
		}
		if err := st.DeleteKeyedAttachmentsExceptContext(
			ctx, plan.messageID, SourceType+":attachment:", keepSourcePartKey,
		); err != nil {
			rowErrs = append(rowErrs, fmt.Errorf("%s record %d: reconcile attachments: %w",
				plan.row.File, plan.row.Record, err))
			continue
		}
		if reference == "" {
			if err := st.RecomputeMessageAttachmentStats(plan.messageID); err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("%s record %d: recompute attachment stats: %w",
					plan.row.File, plan.row.Record, err))
			}
			continue
		}
		// The message row already claims an attachment, so every failure from
		// here on must leave a visible occurrence recording why the bytes are
		// absent instead of leaving a dangling claim.
		fail := func(cause error) {
			rowErrs = append(rowErrs, recordFailedAttachment(ctx, st, plan, reference, cause))
			if err := st.RecomputeMessageAttachmentStats(plan.messageID); err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("%s record %d: recompute attachment stats: %w",
					plan.row.File, plan.row.Record, err))
			}
		}
		sourcePath, found, err := index.resolve(reference)
		if err != nil {
			fail(fmt.Errorf("%s record %d: %w", plan.row.File, plan.row.Record, err))
			continue
		}
		if found {
			info, statErr := os.Lstat(sourcePath)
			switch {
			case statErr == nil && info.Mode().IsRegular() && info.Size() > maxBytes:
				write.Size = info.Size()
				write.State = attachmentpolicy.StateSkipped
				write.SkipReason = attachmentpolicy.SkipSizeCap
				skipped++
			case opts.AttachmentsDir == "":
				fail(fmt.Errorf(
					"%s record %d: %w", plan.row.File, plan.row.Record,
					errors.New("iMazing attachment storage directory is required")))
				continue
			default:
				relPath, contentHash, size, storeErr := export.StoreAttachmentFromPath(
					opts.AttachmentsDir, sourcePath, maxBytes,
				)
				if storeErr == nil {
					write.StoragePath = relPath
					write.ContentHash = contentHash
					write.Size = size
					write.State = attachmentpolicy.StateStored
					write.SkipReason = ""
					stored++
				} else if attachmentSourceUnavailable(sourcePath) {
					found = false
				} else {
					fail(fmt.Errorf("%s record %d: store attachment %q: %w",
						plan.row.File, plan.row.Record, reference, storeErr))
					continue
				}
			}
		}
		if !found {
			missing++
		}
		if err := st.UpsertAttachmentRecordPreservingStored(ctx, plan.messageID, write); err != nil {
			rowErrs = append(rowErrs, fmt.Errorf("%s record %d: persist attachment %q: %w",
				plan.row.File, plan.row.Record, reference, err))
		}
		if err := st.RecomputeMessageAttachmentStats(plan.messageID); err != nil {
			rowErrs = append(rowErrs, fmt.Errorf("%s record %d: recompute attachment stats: %w",
				plan.row.File, plan.row.Record, err))
		}
	}
	return stored, missing, skipped, errors.Join(rowErrs...)
}

// recordFailedAttachment persists a typed failed occurrence for one message
// whose attachment row failed this run, then returns the original cause
// (joined with any persistence failure) so callers can aggregate it while
// still processing the remaining rows.
func recordFailedAttachment(
	ctx context.Context, st *store.Store, plan *plannedMessage, reference string, cause error,
) error {
	write, err := attachmentWrite(plan.row, reference)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("%s record %d: record failed iMazing attachment: %w",
			plan.row.File, plan.row.Record, err))
	}
	if err := st.UpsertAttachmentRecordPreservingStored(ctx, plan.messageID, write); err != nil {
		return errors.Join(cause, fmt.Errorf("%s record %d: persist failed attachment %q: %w",
			plan.row.File, plan.row.Record, reference, err))
	}
	return cause
}

func attachmentWrite(row Row, reference string) (store.AttachmentWrite, error) {
	metadata, err := json.Marshal(map[string]string{"reference": reference})
	if err != nil {
		return store.AttachmentWrite{}, fmt.Errorf("encode attachment metadata: %w", err)
	}
	normalizedReference := strings.ReplaceAll(reference, `\`, "/")
	mimeType := normalizeAttachmentMIME(row.AttachmentType, path.Ext(normalizedReference))
	return store.AttachmentWrite{
		Filename:      path.Base(normalizedReference),
		MIMEType:      mimeType,
		MediaType:     attachmentMediaType(mimeType),
		Metadata:      string(metadata),
		Role:          store.AttachmentRoleStandalone,
		RoleSource:    store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: SourceType + ":attachment:" + stableHash(reference),
		State:         attachmentpolicy.StateFailed,
		SkipReason:    attachmentpolicy.SkipFetchFailure,
	}, nil
}

func normalizeAttachmentMIME(raw, extension string) string {
	if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(raw)); err == nil && strings.Contains(mediaType, "/") {
		return strings.ToLower(mediaType)
	}
	if detected := mime.TypeByExtension(strings.ToLower(extension)); detected != "" {
		if mediaType, _, err := mime.ParseMediaType(detected); err == nil {
			return strings.ToLower(mediaType)
		}
	}
	return "application/octet-stream"
}

// attachmentMediaType maps a normalized MIME type onto the repository's
// attachment categories. Image, video, and audio keep their MIME prefix;
// every other file type — including PDFs and unknown content — is a
// document so document-oriented processing can find it.
func attachmentMediaType(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

func attachmentSourceUnavailable(sourcePath string) bool {
	file, err := os.Open(sourcePath)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
	}
	return file.Close() != nil
}
