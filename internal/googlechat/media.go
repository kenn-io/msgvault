package googlechat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

func localMediaPath(name string) bool {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") || strings.Contains(name, ":") {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(name), "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// openMedia keeps the containment boundary at the conversation, including when
// a path traverses a symlink. No filename-prefix or URL fallback is used.
func (r *exportReader) openMedia(ctx context.Context, group, name string) (file *os.File, retErr error) {
	defer func() {
		if retErr != nil && file != nil {
			retErr = errors.Join(retErr, file.Close())
			file = nil
		}
	}()
	for _, candidate := range r.groups[group] {
		if err := ctx.Err(); err != nil {
			return file, err
		}
		root, err := candidate.part.root.OpenRoot(candidate.dir)
		if err != nil {
			return file, err
		}
		info, statErr := root.Stat(name)
		if statErr == nil && !info.Mode().IsRegular() {
			statErr = &os.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
		}
		if statErr != nil {
			_ = root.Close()
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return file, statErr
		}
		next, openErr := root.Open(name)
		closeErr := root.Close()
		if closeErr != nil {
			if next != nil {
				_ = next.Close()
			}
			return file, closeErr
		}
		if errors.Is(openErr, fs.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return file, openErr
		}
		if file != nil {
			_ = next.Close()
			return file, fmt.Errorf("googlechat: ambiguous media %q in %s across supplied parts", name, group)
		}
		file = next
	}
	if file == nil {
		return nil, fs.ErrNotExist
	}
	return file, nil
}

func mediaWrites(ctx context.Context, st *store.Store, reader *exportReader, group string, messageID int64, m *message, opts ImportOptions, summary *ImportSummary) ([]store.AttachmentWrite, error) {
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
	for index, att := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := "file:" + strconv.Itoa(index)
		if messageID != 0 {
			previous, ok := existing[key]
			if !ok || previous.ContentHash != "" {
				continue
			}
		}
		metadata, err := json.Marshal(att)
		if err != nil {
			return nil, err
		}
		write := store.AttachmentWrite{SourcePartKey: key, Filename: att.Original, Metadata: string(metadata), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
		if write.Filename == "" {
			write.Filename = filepath.Base(att.Export)
		}
		write.MIMEType = mime.TypeByExtension(strings.ToLower(filepath.Ext(write.Filename)))
		if write.MIMEType == "" {
			write.MIMEType = "application/octet-stream"
		}
		if !localMediaPath(att.Export) {
			summary.AttachmentsUnsupported++
		} else if opts.AttachmentsDir != "" {
			file, err := reader.openMedia(ctx, group, att.Export)
			if err == nil {
				info, statErr := file.Stat()
				if statErr != nil {
					_ = file.Close()
					return nil, statErr
				}
				switch {
				case !info.Mode().IsRegular():
					summary.AttachmentsUnsupported++
				case info.Size() > attachmentpolicy.DefaultChatMaxBytes:
					summary.AttachmentsOversized++
				default:
					write.StoragePath, write.ContentHash, write.Size, err = export.StoreAttachmentStream(ctx, opts.AttachmentsDir, file, attachmentpolicy.DefaultChatMaxBytes)
					if errors.Is(err, export.ErrAttachmentTooLarge) {
						summary.AttachmentsOversized++
						err = nil
					}
				}
				err = errors.Join(err, file.Close())
				if err != nil {
					return nil, fmt.Errorf("googlechat: store media %q: %w", att.Export, err)
				}
			} else {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				// Source path failures are missing media; ambiguity is a fatal choice of
				// bytes. Storage failures above always stop the import.
				if _, ok := errors.AsType[*os.PathError](err); !ok && !errors.Is(err, fs.ErrNotExist) {
					return nil, err
				}
			}
		}
		if write.ContentHash == "" {
			summary.AttachmentsMissing++
			write.State = attachmentpolicy.StateFailed
			write.SkipReason = attachmentpolicy.SkipSourceUnavailable
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
