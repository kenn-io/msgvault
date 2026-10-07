package chatwoot

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var errMediaFetch = errors.New("read Chatwoot media failed")

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

func isAudio(a Attachment) bool {
	return a.FileType == "audio" || strings.HasPrefix(a.ContentType, "audio/")
}

// persistMedia returns the latest eligible failure start and whether files are pending.
func (imp *Importer) persistMedia(ctx context.Context, messageID int64, attachments []Attachment, opts ImportOptions, sum *ImportSummary, reusable map[string]store.AttachmentRef) (failedSince int64, waiting bool, err error) {
	existing, err := imp.store.MessageChatwootAttachments(messageID)
	if err != nil {
		return 0, false, err
	}
	own := maps.Clone(existing)
	for key, ref := range reusable {
		if _, ok := existing[key]; !ok {
			existing[key] = ref
		}
	}
	refs := make([]store.AttachmentRef, 0, len(attachments))
	for index, a := range attachments {
		if err = ctx.Err(); err != nil {
			return 0, false, err
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
		if previous.SkipReason == attachmentpolicy.SkipSizeCap && storedMediaIdentity(previous) == currentURL {
			ref.Size = max(ref.Size, previous.Size)
			if ref.MimeType == "" {
				ref.MimeType = previous.MimeType
			}
		}
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
		var failure struct {
			FailedURL   string `json:"failed_url"`
			FailedSince int64  `json:"failed_since"`
		}
		for _, candidate := range existing {
			var saved struct {
				FailedURL   string `json:"failed_url"`
				FailedSince int64  `json:"failed_since"`
			}
			if json.Unmarshal([]byte(candidate.Metadata), &saved) == nil && saved.FailedSince > 0 &&
				(saved.FailedURL == currentURL || (currentURL == "" && candidate.SourceAttachmentID == key)) &&
				(failure.FailedSince == 0 || saved.FailedSince < failure.FailedSince) {
				failure = saved
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
		maxBytes := opts.Policy.MaxBytes
		if maxBytes <= 0 {
			maxBytes = attachmentpolicy.DefaultChatMaxBytes
		}
		reason := opts.Policy.Evaluate(attachmentpolicy.Conversation{}, int64(ref.Size))
		switch {
		case unchanged:
			ref.State = attachmentpolicy.StateStored
			if currentURL == storedURL && currentURL != "" {
				failure.FailedSince, failure.FailedURL = 0, ""
			}
		case reason != "":
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = reason
		case opts.NoMedia && a.FileType != "location":
			waiting = true
		case a.FileType != "location" && remote == "":
			waiting = true
		case a.FileType == "location" || opts.AttachmentsDir == "":
		case failure.FailedSince > 0 && now().Sub(time.Unix(failure.FailedSince, 0)) >= artifactWindow && !opts.Full:
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
		default:
			storage, hash, size, mimeType, fetchErr := imp.downloadMedia(ctx, remote, opts.AttachmentsDir, maxBytes, ref.MimeType)
			ref.MimeType = mimeType
			if fetchErr == nil {
				ref.StoragePath = storage
				ref.ContentHash = hash
				ref.Size = size
				ref.State = attachmentpolicy.StateStored
				storedURL = currentURL
				failure.FailedSince, failure.FailedURL = 0, ""
			} else {
				if ctx.Err() != nil {
					return 0, false, ctx.Err()
				}
				if errors.Is(fetchErr, export.ErrAttachmentTooLarge) {
					ref.State = attachmentpolicy.StateSkipped
					ref.SkipReason = attachmentpolicy.SkipSizeCap
					if !stored {
						ref.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, int64(ref.Size))
					}
				} else if errors.Is(fetchErr, errMediaFetch) {
					ref.State = attachmentpolicy.StateFailed
					ref.SkipReason = attachmentpolicy.SkipFetchFailure
					sum.MediaFailures++
					failure.FailedURL = currentURL
					if failure.FailedSince == 0 {
						failure.FailedSince = now().Unix()
					}
				} else {
					return 0, false, fetchErr
				}
				// Retain the successful URL separately from current evidence. Otherwise a
				// failed replacement would falsely appear downloaded on the next sync.
			}
		}
		if attachmentpolicy.RetryEligible(ref.State) && failure.FailedSince > 0 && now().Sub(time.Unix(failure.FailedSince, 0)) < artifactWindow {
			failedSince = max(failedSince, failure.FailedSince)
		}
		metadata, marshalErr := json.Marshal(map[string]any{"provider": SourceType, "source": a.Raw, "url": remote, "stored_url": storedURL, "failed_url": failure.FailedURL, "failed_since": failure.FailedSince, "source_transcript": map[string]string{"provider": SourceType, "text": a.TranscribedText}}, json.Deterministic(true))
		if marshalErr != nil {
			return 0, false, marshalErr
		}
		ref.Metadata = string(metadata)
		refs = append(refs, ref)
	}
	// The archive keeps stored bytes the provider no longer lists, unless a
	// current occurrence already carries the same content.
	current := map[string]bool{}
	for _, ref := range refs {
		current[ref.SourceAttachmentID] = true
		current[ref.ContentHash] = ref.ContentHash != ""
	}
	for _, key := range slices.Sorted(maps.Keys(own)) {
		if ref := own[key]; !current[key] && ref.ContentHash != "" && !current[ref.ContentHash] {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 && len(existing) == 0 {
		return failedSince, waiting, nil
	}
	if err = imp.store.ReplaceMessageChatwootAttachments(messageID, refs); err != nil {
		return 0, false, err
	}
	return failedSince, waiting, imp.store.RecomputeMessageAttachmentStats(messageID)
}

func (imp *Importer) downloadMedia(ctx context.Context, remote, dir string, maxBytes int64, mimeType string) (string, string, int, string, error) {
	body, length, reportedType, err := imp.client.OpenMedia(ctx, remote, maxBytes)
	if err != nil {
		return "", "", 0, mimeType, errMediaFetch
	}
	defer func() { _ = body.Close() }()
	if mimeType == "" {
		mimeType = strings.Split(reportedType, ";")[0]
	}
	if length > maxBytes {
		return "", "", 0, mimeType, export.ErrAttachmentTooLarge
	}
	source := &export.SourceReader{R: body}
	storage, hash, size, err := export.StoreAttachmentStream(ctx, dir, source, maxBytes)
	if err != nil && source.Err != nil {
		err = errMediaFetch
	}
	return storage, hash, int(size), mimeType, err
}
