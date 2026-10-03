package chatwoot

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var errMediaTooLarge = errors.New("chatwoot media exceeds configured size cap")

func attachmentKey(a Attachment, index int) string {
	if a.ID < 0 {
		return "chatwoot:recording:" + strconv.FormatInt(-a.ID, 10)
	}
	if a.ID > 0 {
		return "chatwoot:attachment:" + strconv.FormatInt(a.ID, 10)
	}
	return "chatwoot:part:" + strconv.Itoa(index)
}

// mediaIdentity removes expiring signatures while retaining resource selectors.
func mediaIdentity(remote string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return remote
	}
	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-amz-") || strings.HasPrefix(lower, "x-goog-") || lower == "signature" || lower == "expires" || lower == "awsaccesskeyid" || lower == "googleaccessid" {
			query.Del(key)
		}
	}
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return u.String()
}

func storedMediaIdentity(ref store.AttachmentRef) string {
	var metadata struct {
		StoredURL string `json:"stored_url"`
		URL       string `json:"url"`
	}
	if json.Unmarshal([]byte(ref.Metadata), &metadata) != nil {
		return ""
	}
	if metadata.StoredURL != "" {
		return metadata.StoredURL
	}
	return mediaIdentity(metadata.URL)
}

func (imp *Importer) persistMedia(ctx context.Context, messageID int64, attachments []Attachment, opts ImportOptions, sum *ImportSummary, reusable map[string]store.AttachmentRef) error {
	existing, err := imp.store.MessageChatwootAttachments(messageID)
	if err != nil {
		return err
	}
	for key, ref := range reusable {
		if _, ok := existing[key]; !ok {
			existing[key] = ref
		}
	}
	refs := make([]store.AttachmentRef, 0, len(attachments))
	for index, a := range attachments {
		if err = ctx.Err(); err != nil {
			return err
		}
		key := attachmentKey(a, index)
		remote := a.DataURL
		if remote == "" {
			remote = a.ExternalURL
		}
		currentURL := mediaIdentity(remote)
		filename := fmt.Sprintf("attachment-%d", a.ID)
		if parsed, parseErr := url.Parse(remote); parseErr == nil && path.Base(parsed.Path) != "." && path.Base(parsed.Path) != "/" {
			filename = path.Base(parsed.Path)
		}
		if path.Ext(filename) == "" && a.Extension != "" {
			filename += "." + strings.TrimPrefix(a.Extension, ".")
		}
		maxInt := int64(^uint(0) >> 1)
		ref := store.AttachmentRef{Filename: filename, MimeType: a.ContentType, SourceAttachmentID: key, SourcePartKey: key, StoragePath: key, Size: int(min(max(a.FileSize, 0), maxInt)), MediaType: a.FileType, Width: int64(a.Width), Height: int64(a.Height), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StatePending}
		previous, hadPrevious := existing[key]
		// A live call recording can later appear as a provider attachment. Keep
		// its stored bytes across occurrence IDs while retaining current metadata.
		if currentURL != "" && (previous.ContentHash == "" || storedMediaIdentity(previous) != currentURL) {
			for _, candidate := range existing {
				if candidate.ContentHash != "" && storedMediaIdentity(candidate) == currentURL {
					previous, hadPrevious = candidate, true
					break
				}
			}
		}
		storedURL := ""
		retain := func() {
			ref.StoragePath = previous.StoragePath
			ref.ContentHash = previous.ContentHash
			ref.Size = previous.Size
			if ref.MimeType == "" {
				ref.MimeType = previous.MimeType
			}
		}
		stored := hadPrevious && previous.ContentHash != ""
		if stored {
			retain()
			storedURL = storedMediaIdentity(previous)
		}
		unchanged := stored && (remote == "" || currentURL == storedURL)
		maxBytes := opts.MaxMediaBytes
		if maxBytes <= 0 {
			maxBytes = attachmentpolicy.DefaultChatMaxBytes
		}
		switch {
		case unchanged:
			ref.State = attachmentpolicy.StateStored
		case opts.NoMedia || !opts.Media:
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = attachmentpolicy.SkipAccountPolicy
		case a.FileSize > maxBytes:
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = attachmentpolicy.SkipSizeCap
		case a.FileType == "location" || remote == "" || opts.AttachmentsDir == "":
		default:
			storage, hash, size, mimeType, fetchErr := imp.downloadMedia(ctx, remote, opts.AttachmentsDir, maxBytes, ref.MimeType)
			if fetchErr == nil {
				ref.StoragePath = storage
				ref.ContentHash = hash
				ref.Size = size
				ref.MimeType = mimeType
				ref.State = attachmentpolicy.StateStored
				storedURL = currentURL
			} else {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(fetchErr, errMediaTooLarge) {
					ref.State = attachmentpolicy.StateSkipped
					ref.SkipReason = attachmentpolicy.SkipSizeCap
					if !stored {
						ref.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, int64(ref.Size))
					}
				} else {
					ref.State = attachmentpolicy.StateFailed
					ref.SkipReason = attachmentpolicy.SkipFetchFailure
					sum.MediaFailures++
				}
				// Retain the successful URL separately from current evidence. Otherwise a
				// failed replacement would falsely appear downloaded on the next sync.
			}
		}
		metadata, marshalErr := json.Marshal(map[string]any{"provider": SourceType, "source": a.Raw, "url": remote, "stored_url": storedURL, "source_transcript": map[string]string{"provider": SourceType, "text": a.TranscribedText}}, json.Deterministic(true))
		if marshalErr != nil {
			return marshalErr
		}
		ref.Metadata = string(metadata)
		refs = append(refs, ref)
	}
	if len(refs) == 0 && len(existing) == 0 {
		return nil
	}
	if err = imp.store.ReplaceMessageChatwootAttachments(messageID, refs); err != nil {
		return err
	}
	return imp.store.RecomputeMessageAttachmentStats(messageID)
}

func (imp *Importer) downloadMedia(ctx context.Context, remote, dir string, maxBytes int64, mimeType string) (string, string, int, string, error) {
	if maxBytes == math.MaxInt64 {
		maxBytes--
	}
	body, length, reportedType, err := imp.client.OpenMedia(ctx, remote, maxBytes)
	if err != nil {
		return "", "", 0, mimeType, err
	}
	defer func() { _ = body.Close() }()
	if length > maxBytes {
		return "", "", 0, mimeType, errMediaTooLarge
	}
	file, err := os.CreateTemp("", "msgvault-chatwoot-media-*")
	if err != nil {
		return "", "", 0, mimeType, err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	n, err := io.Copy(file, io.LimitReader(body, maxBytes+1))
	if err != nil {
		return "", "", 0, mimeType, errors.New("read Chatwoot media failed")
	}
	if n > maxBytes {
		return "", "", 0, mimeType, errMediaTooLarge
	}
	if err = ctx.Err(); err != nil {
		return "", "", 0, mimeType, err
	}
	if err = file.Close(); err != nil {
		return "", "", 0, mimeType, err
	}
	storage, hash, size, err := export.StoreAttachmentFromPath(dir, file.Name(), maxBytes)
	if mimeType == "" {
		mimeType = strings.Split(reportedType, ";")[0]
	}
	return storage, hash, int(size), mimeType, err
}
