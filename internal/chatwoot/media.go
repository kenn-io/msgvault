package chatwoot

import (
	"context"
	"encoding/json/jsontext"
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
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

var errMediaFetch = errors.New("read Chatwoot media failed")

type mediaMetadata struct {
	FailedSince      int64          `json:"failed_since"`
	FailedURL        string         `json:"failed_url"`
	Provider         string         `json:"provider"`
	RejectedSize     int64          `json:"rejected_size"`
	RejectedURL      string         `json:"rejected_url"`
	Source           jsontext.Value `json:"source"`
	SourceTranscript struct {
		Provider string `json:"provider"`
		Text     string `json:"text"`
	} `json:"source_transcript"`
	StoredURL string `json:"stored_url"`
	URL       string `json:"url"`
}

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
	var metadata mediaMetadata
	if json.Unmarshal([]byte(ref.Metadata), &metadata) != nil {
		return ""
	}
	if metadata.StoredURL != "" {
		return metadata.StoredURL
	}
	return mediaIdentity(metadata.URL)
}

func retryingSince(ref store.AttachmentRef) (int64, string) {
	var evidence mediaMetadata
	if json.Unmarshal([]byte(ref.Metadata), &evidence) == nil && attachmentpolicy.RetryEligible(ref.State) &&
		evidence.FailedSince > 0 && now().Sub(time.Unix(evidence.FailedSince, 0)) < artifactWindow {
		return evidence.FailedSince, evidence.FailedURL
	}
	return 0, ""
}

func isAudio(a Attachment) bool {
	return a.FileType == "audio" || strings.HasPrefix(a.ContentType, "audio/")
}

// persistMedia returns the latest eligible failure start and whether files are pending.
func (imp *Importer) persistMedia(ctx context.Context, messageID int64, attachments []Attachment, opts ImportOptions, sum *ImportSummary, reusable map[string]store.AttachmentRef) (failedSince int64, waiting bool, err error) {
	existing, err := imp.store.MessageProviderAttachments(messageID, "chatwoot:")
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
	current := map[string]bool{}
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
		if previous.ContentHash == "" && previous.SkipReason == attachmentpolicy.SkipSizeCap && storedMediaIdentity(previous) == currentURL {
			ref.Size = max(ref.Size, previous.Size)
			if ref.MimeType == "" {
				ref.MimeType = previous.MimeType
			}
		}
		// A live call recording can later appear as a provider attachment. Keep
		// its stored bytes across occurrence IDs while retaining current metadata.
		if currentURL != "" && (previous.ContentHash == "" || storedMediaIdentity(previous) != currentURL) {
		findStored:
			for _, candidates := range []map[string]store.AttachmentRef{existing, reusable} {
				for _, candidate := range candidates {
					if candidate.ContentHash != "" && storedMediaIdentity(candidate) == currentURL {
						previous, hadPrevious = candidate, true
						break findStored
					}
				}
			}
		}
		var evidence mediaMetadata
		for _, candidates := range []map[string]store.AttachmentRef{existing, reusable} {
			for _, candidate := range candidates {
				var saved mediaMetadata
				if json.Unmarshal([]byte(candidate.Metadata), &saved) != nil {
					continue
				}
				if saved.RejectedSize > 0 &&
					(saved.RejectedURL == currentURL || (currentURL == "" && candidate.SourceAttachmentID == key)) && saved.RejectedSize > evidence.RejectedSize {
					evidence.RejectedURL, evidence.RejectedSize = saved.RejectedURL, saved.RejectedSize
				}
				if saved.FailedSince > 0 &&
					(saved.FailedURL == currentURL || (currentURL == "" && candidate.SourceAttachmentID == key)) &&
					(evidence.FailedSince == 0 || saved.FailedSince < evidence.FailedSince) {
					evidence.FailedURL, evidence.FailedSince = saved.FailedURL, saved.FailedSince
				}
			}
		}
		proposedSize := int64(ref.Size)
		if currentURL != "" && evidence.RejectedURL == currentURL {
			proposedSize = max(proposedSize, evidence.RejectedSize)
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
		policy := opts.Policy
		policy.MaxBytes = maxBytes
		reason := policy.Evaluate(attachmentpolicy.Conversation{}, proposedSize)
		switch {
		case remote == "" && evidence.FailedSince > 0:
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
		case unchanged:
			ref.State = attachmentpolicy.StateStored
			if currentURL == storedURL && currentURL != "" {
				evidence.FailedSince, evidence.FailedURL = 0, ""
				evidence.RejectedURL, evidence.RejectedSize = "", 0
			}
		case reason != "":
			if reason == attachmentpolicy.SkipSizeCap && currentURL != "" {
				evidence.RejectedURL, evidence.RejectedSize = currentURL, int64(attachmentpolicy.OversizeMarkerSize(maxBytes, proposedSize))
			}
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = reason
		case opts.NoMedia && a.FileType != "location":
			waiting = true
		case a.FileType != "location" && remote == "":
			waiting = true
		case a.FileType == "location" || opts.AttachmentsDir == "":
		case evidence.FailedSince > 0 && now().Sub(time.Unix(evidence.FailedSince, 0)) >= artifactWindow && !opts.Full:
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
		case imp.failedMedia[currentURL] > 0:
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
			evidence.FailedURL = currentURL
			if failedAt := imp.failedMedia[currentURL]; evidence.FailedSince == 0 || failedAt < evidence.FailedSince {
				evidence.FailedSince = failedAt
			}
		default:
			storage, hash, size, mimeType, fetchErr := imp.downloadMedia(ctx, remote, opts.AttachmentsDir, maxBytes, ref.MimeType)
			ref.MimeType = mimeType
			if fetchErr == nil {
				ref.StoragePath = storage
				ref.ContentHash = hash
				ref.Size = size
				ref.State = attachmentpolicy.StateStored
				storedURL = currentURL
				evidence.FailedSince, evidence.FailedURL = 0, ""
				evidence.RejectedURL, evidence.RejectedSize = "", 0
			} else {
				if ctx.Err() != nil {
					return 0, false, ctx.Err()
				}
				if errors.Is(fetchErr, export.ErrAttachmentTooLarge) {
					ref.State = attachmentpolicy.StateSkipped
					ref.SkipReason = attachmentpolicy.SkipSizeCap
					evidence.RejectedURL, evidence.RejectedSize = currentURL, int64(attachmentpolicy.OversizeMarkerSize(maxBytes, max(proposedSize, int64(size))))
					if !stored {
						ref.Size = int(evidence.RejectedSize)
					}
				} else if errors.Is(fetchErr, errMediaFetch) {
					ref.State = attachmentpolicy.StateFailed
					ref.SkipReason = attachmentpolicy.SkipFetchFailure
					sum.MediaFailures++
					evidence.FailedURL = currentURL
					if evidence.FailedSince == 0 {
						evidence.FailedSince = now().Unix()
					}
					imp.failedMedia[currentURL] = evidence.FailedSince
				} else {
					return 0, false, fetchErr
				}
				// Retain the successful URL separately from current evidence. Otherwise a
				// failed replacement would falsely appear downloaded on the next sync.
			}
		}
		evidence.Provider, evidence.Source = SourceType, a.Raw
		evidence.URL, evidence.StoredURL = remote, storedURL
		evidence.SourceTranscript.Provider, evidence.SourceTranscript.Text = SourceType, a.TranscribedText
		metadata, marshalErr := json.Marshal(evidence, json.Deterministic(true))
		if marshalErr != nil {
			return 0, false, marshalErr
		}
		ref.Metadata = string(metadata)
		refs = append(refs, ref)
		current[key] = true
		if ref.ContentHash != "" {
			current[ref.ContentHash] = true
		}
		if currentURL != "" {
			current[currentURL] = true
		}
	}
	// Keep unlisted stored bytes or live failures unless a current occurrence supersedes them.
	for _, key := range slices.Sorted(maps.Keys(own)) {
		ref := own[key]
		since, identity := retryingSince(ref)
		if !current[key] && ((ref.ContentHash != "" && !current[ref.ContentHash]) ||
			(ref.ContentHash == "" && since > 0 && (identity == "" || !current[identity]))) {
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		since, _ := retryingSince(ref)
		failedSince = max(failedSince, since)
	}
	unchanged := len(refs) == len(own)
	for _, ref := range refs {
		previous, ok := own[ref.SourceAttachmentID]
		if !ok || ref.SourcePartKey == "" || ref.Role == "" || ref.RoleSource == "" ||
			!meetingarchive.JSONEvidenceEqual([]byte(previous.Metadata), []byte(ref.Metadata)) {
			unchanged = false
			break
		}
		ref.Metadata, previous.Metadata = "", ""
		if ref != previous {
			unchanged = false
			break
		}
	}
	if unchanged {
		return failedSince, waiting, nil
	}
	return failedSince, waiting, imp.store.ReplaceMessageChatwootAttachments(messageID, refs)
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
		return "", "", attachmentpolicy.OversizeMarkerSize(maxBytes, length), mimeType, export.ErrAttachmentTooLarge
	}
	source := &export.SourceReader{R: body}
	storage, hash, size, err := export.StoreAttachmentStream(ctx, dir, source, maxBytes)
	if err != nil && source.Err != nil {
		err = errMediaFetch
	}
	return storage, hash, int(size), mimeType, err
}
