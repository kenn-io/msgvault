package remoteimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/net/html"
)

const (
	maxArchiveImages    = 64
	maxArchiveBytes     = 30 << 20
	maxArchiveHTMLBytes = 8 << 20
	archiveTimeout      = time.Minute
)

// ArchiveResult reports best-effort image archiving, independently of mail
// ingestion. Errors contain no sender URLs or query parameters.
type ArchiveResult struct {
	Downloaded int
	Reused     int
	Errors     []error
}

func imageKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return "remote-image:" + hex.EncodeToString(sum[:])
}

func remoteURL(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "//") {
		value = "https:" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// mapImageSources preserves the original HTML bytes except for successful
// img src replacements. Tokenization never evaluates HTML or loads resources.
func mapImageSources(source string, replace func(string) string) string {
	var out strings.Builder
	tokens := html.NewTokenizer(strings.NewReader(source))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		raw := string(tokens.Raw())
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			out.WriteString(raw)
			continue
		}
		token := tokens.Token()
		changed := false
		if token.Data == "img" {
			for i := range token.Attr {
				if token.Attr[i].Key != "src" {
					continue
				}
				target := remoteURL(token.Attr[i].Val)
				if target == "" {
					continue
				}
				if replacement := replace(target); replacement != "" {
					token.Attr[i].Val = replacement
					changed = true
				}
			}
		}
		if changed {
			out.WriteString(token.String())
		} else {
			out.WriteString(raw)
		}
	}
	return out.String()
}

// RewriteHTML substitutes only already archived, message-scoped image refs.
// It performs no I/O and leaves the stored HTML and original MIME unchanged.
func RewriteHTML(source string, refs map[string]store.AttachmentRef) string {
	if len(refs) == 0 || len(source) > maxArchiveHTMLBytes {
		return source
	}
	return mapImageSources(source, func(target string) string {
		key := imageKey(target)
		if ref, ok := refs[key]; ok && ref.ContentID == key && ref.StoragePath != "" && ref.ContentHash != "" {
			return "cid:" + key
		}
		return ""
	})
}

// Archive must only be called after explicit opt-in. Network and durable file
// writes happen outside database transactions. Each URL has its own stable
// occurrence identity even when different URLs share the same content bytes.
func (f *Fetcher) Archive(ctx context.Context, st *store.Store, dir string, messageID int64, source string) ArchiveResult {
	var result ArchiveResult
	if err := ctx.Err(); err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}
	if dir == "" {
		result.Errors = append(result.Errors, errors.New("remote image storage directory is not configured"))
		return result
	}
	if len(source) > maxArchiveHTMLBytes {
		result.Errors = append(result.Errors, errors.New("remote image HTML exceeds size limit"))
		return result
	}
	if source == "" {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	refs, err := st.MessageRemoteImages(messageID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("load archived remote images: %w", err))
		return result
	}
	var targets []string
	seen := map[string]bool{}
	truncated := false
	mapImageSources(source, func(target string) string {
		if seen[target] {
			return ""
		}
		if len(targets) >= maxArchiveImages {
			truncated = true
			return ""
		}
		seen[target] = true
		targets = append(targets, target)
		return ""
	})
	if truncated {
		result.Errors = append(result.Errors, errors.New("remote image count exceeds limit"))
	}
	total := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			result.Errors = append(result.Errors, err)
			break
		}
		key := imageKey(target)
		if ref, ok := refs[key]; ok && ref.ContentID == key && ref.ContentHash != "" && ref.StoragePath != "" {
			result.Reused++
			continue
		}
		if total >= maxArchiveBytes {
			result.Errors = append(result.Errors, errors.New("remote image total bytes exceeds limit"))
			break
		}
		fetchCtx, fetchCancel := context.WithTimeout(ctx, Timeout)
		contentType, body, fetchErr := f.Fetch(fetchCtx, target)
		fetchCancel()
		if fetchErr != nil {
			result.Errors = append(result.Errors, fetchErr)
			continue
		}
		total += len(body)
		if total > maxArchiveBytes {
			result.Errors = append(result.Errors, errors.New("remote image total bytes exceeds limit"))
			break
		}
		if contentType == "image/jpg" {
			contentType = "image/jpeg"
		}
		if !permittedRaster(contentType, body) {
			result.Errors = append(result.Errors, errors.New("remote content is not a supported raster image"))
			continue
		}
		att := &mime.Attachment{ContentType: contentType, Content: body}
		receipt, err := export.StoreAttachmentFileDurable(dir, att)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("store remote image bytes: %w", err))
			continue
		}
		err = st.UpsertRemoteImageAttachment(ctx, messageID, store.AttachmentWrite{
			Filename: "remote-image-" + strings.TrimPrefix(key, "remote-image:") + rasterExtension(contentType),
			MIMEType: contentType, StoragePath: receipt.StoragePath, ContentHash: receipt.ContentHash, Size: int64(len(body)),
			SourceAttachmentID: key, SourcePartKey: key, ContentID: key, MediaType: "image",
			Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("record remote image: %w", err))
			continue
		}
		result.Downloaded++
	}
	if result.Downloaded > 0 || result.Reused > 0 {
		if err := st.RecomputeMessageAttachmentStats(messageID); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("update remote image metadata: %w", err))
		}
	}
	return result
}

func rasterExtension(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}

func permittedRaster(contentType string, body []byte) bool {
	return rasterExtension(contentType) != "" && len(body) > 0 && http.DetectContentType(body) == contentType
}
