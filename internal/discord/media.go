package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

var (
	// ErrInvalidMediaURL classifies attachment URLs outside Discord's approved
	// CDN origins and path shape.
	ErrInvalidMediaURL = errors.New("invalid Discord attachment URL")
	// ErrMediaTooLarge classifies attachments that exceed the configured cap.
	ErrMediaTooLarge = errors.New("discord attachment exceeds the configured size cap")
	// ErrMediaDownload classifies a CDN status or transport failure.
	ErrMediaDownload = errors.New("discord attachment download failed")
	// ErrMediaRedirect classifies redirects refused by the attachment client.
	ErrMediaRedirect = errors.New("discord attachment redirects are not allowed")
	// ErrMediaStorage classifies temporary-file or content-store failures.
	ErrMediaStorage = errors.New("discord attachment storage failed")
	// ErrMediaRefresh classifies a transient failure to refresh a source message.
	ErrMediaRefresh = errors.New("discord attachment URL refresh failed")
	// ErrMediaUnrecoverable means the source message or stable attachment is gone.
	ErrMediaUnrecoverable = errors.New("discord attachment is no longer recoverable")
)

const mediaHTTPTimeout = 60 * time.Second

// MediaOutcome describes the durable state of one Discord attachment after a
// persistence or backfill attempt.
type MediaOutcome string

const (
	MediaDownloaded    MediaOutcome = "downloaded"
	MediaPending       MediaOutcome = "pending"
	MediaUnrecoverable MediaOutcome = "unrecoverable"
)

// MediaItemResult reports binary work without turning it into a message
// persistence error. Err is deliberately limited to stable local sentinels and
// context errors; it never contains an attachment URL or signed query string.
type MediaItemResult struct {
	SourceAttachmentID string
	Outcome            MediaOutcome
	Err                error
}

// MediaResult contains the outcome of each current or pending attachment.
type MediaResult struct {
	Items []MediaItemResult
}

// MediaArchiver persists Discord attachment markers and best-effort binary
// content. Its production URL policy is fixed rather than caller-configurable.
type MediaArchiver struct {
	store               *store.Store
	api                 API
	attachmentsDir      string
	maxBytes            int64
	httpClient          *http.Client
	allowedOrigins      map[string]struct{}
	storeAttachmentFile func(string, *mime.Attachment) (string, error)
}

// NewMediaArchiver constructs the production attachment boundary. A nonpositive
// cap uses the configured Discord default of 50 MiB.
func NewMediaArchiver(
	st *store.Store, api API, attachmentsDir string, maxBytes int64,
) (*MediaArchiver, error) {
	if st == nil {
		return nil, errors.New("discord media store is required")
	}
	if attachmentsDir == "" {
		return nil, errors.New("discord attachments directory is required")
	}
	if maxBytes <= 0 {
		maxBytes = config.DefaultDiscordMaxMediaBytes
	}
	return &MediaArchiver{
		store:               st,
		api:                 api,
		attachmentsDir:      attachmentsDir,
		maxBytes:            maxBytes,
		httpClient:          mediaHTTPClient(),
		storeAttachmentFile: export.StoreAttachmentFile,
		allowedOrigins: map[string]struct{}{
			"https://cdn.discordapp.com":   {},
			"https://media.discordapp.net": {},
		},
	}, nil
}

// newTestMediaArchiver is the only alternate-origin injection point. It is
// intentionally unexported and accepts only an exact loopback HTTP(S) origin,
// keeping production callers on the fixed Discord CDN allowlist.
func newTestMediaArchiver(
	st *store.Store, api API, attachmentsDir string, maxBytes int64, testOrigin string,
) (*MediaArchiver, error) {
	archiver, err := NewMediaArchiver(st, api, attachmentsDir, maxBytes)
	if err != nil {
		return nil, err
	}
	origin, err := parseLoopbackTestOrigin(testOrigin)
	if err != nil {
		return nil, err
	}
	archiver.allowedOrigins = map[string]struct{}{origin: {}}
	return archiver, nil
}

func mediaHTTPClient() *http.Client {
	return &http.Client{
		Timeout: mediaHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return ErrMediaRedirect
		},
	}
}

func parseLoopbackTestOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("invalid Discord media test origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("discord media test origin must use HTTP or HTTPS")
	}
	if !isLoopbackMediaHost(parsed.Hostname()) {
		return "", errors.New("discord media test origin must be loopback")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func isLoopbackMediaHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PersistAttachments records every current attachment as a pending marker
// before attempting binary work. Download, cap, cancellation, and filesystem
// failures remain per-item pending outcomes and do not become message errors.
func (m *MediaArchiver) PersistAttachments(
	ctx context.Context, messageID int64, attachments []Attachment,
) (MediaResult, error) {
	return m.persistAttachments(ctx, messageID, attachments, true)
}

// persistAttachments refreshes the complete observed attachment set. When
// retryExisting is false, known pending rows get fresh metadata without a
// duplicate download attempt; newly observed rows are still attempted.
func (m *MediaArchiver) persistAttachments(
	ctx context.Context, messageID int64, attachments []Attachment, retryExisting bool,
) (MediaResult, error) {
	existing, err := m.store.MessageDiscordAttachments(messageID)
	if err != nil {
		return MediaResult{}, fmt.Errorf("load Discord attachment metadata: %w", err)
	}

	refs := mapAttachments(attachments)
	if len(refs) != len(attachments) {
		return MediaResult{}, errors.New("map Discord attachment metadata: attachment count changed")
	}
	type attachmentWork struct {
		attachment Attachment
		ref        *store.AttachmentRef
		download   bool
		report     bool
	}
	work := make([]attachmentWork, 0, len(attachments))
	remainingRefs := refs
	for _, attachment := range attachments {
		if len(remainingRefs) == 0 {
			return MediaResult{}, errors.New("map Discord attachment metadata: attachment count changed")
		}
		ref := &remainingRefs[0]
		remainingRefs = remainingRefs[1:]
		item := attachmentWork{attachment: attachment, ref: ref, download: true, report: true}
		if previous, ok := existing[ref.SourceAttachmentID]; ok {
			if store.IsDiscordAttachmentDownloaded(previous) {
				ref.StoragePath = previous.StoragePath
				ref.ContentHash = previous.ContentHash
				item.download = false
				item.report = retryExisting
			} else if !retryExisting {
				item.download = false
				item.report = false
			}
		}
		work = append(work, item)
	}
	if err := m.replaceMetadata(messageID, refs); err != nil {
		return MediaResult{}, err
	}

	result := MediaResult{Items: make([]MediaItemResult, 0, len(refs))}
	for _, pending := range work {
		if !pending.report {
			continue
		}
		item := MediaItemResult{SourceAttachmentID: pending.ref.SourceAttachmentID}
		if !pending.download {
			item.Outcome = MediaDownloaded
			result.Items = append(result.Items, item)
			continue
		}
		stored, downloadErr := m.downloadAttachment(ctx, pending.attachment, *pending.ref)
		if downloadErr != nil {
			item.Outcome = MediaPending
			item.Err = downloadErr
			result.Items = append(result.Items, item)
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			item.Outcome = MediaPending
			item.Err = ctxErr
			result.Items = append(result.Items, item)
			continue
		}
		*pending.ref = stored
		if err := m.replaceMetadata(messageID, refs); err != nil {
			item.Outcome = MediaPending
			item.Err = ErrMediaStorage
			result.Items = append(result.Items, item)
			return result, err
		}
		item.Outcome = MediaDownloaded
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// BackfillMessage refreshes one source message before retrying its pending
// attachments. Stable source attachment IDs, not filenames or expiring URLs,
// are used for matching. Gone messages and absent attachment IDs are reported
// distinctly as unrecoverable while their archived marker remains intact.
func (m *MediaArchiver) BackfillMessage(
	ctx context.Context, messageID int64, channelID, sourceMessageID string,
) (MediaResult, error) {
	existing, err := m.store.MessageDiscordAttachments(messageID)
	if err != nil {
		return MediaResult{}, fmt.Errorf("load pending Discord attachment metadata: %w", err)
	}
	pendingIDs := make([]string, 0, len(existing))
	for sourceAttachmentID, ref := range existing {
		if !store.IsDiscordAttachmentDownloaded(ref) {
			pendingIDs = append(pendingIDs, sourceAttachmentID)
		}
	}
	sort.Strings(pendingIDs)
	if len(pendingIDs) == 0 {
		return MediaResult{}, nil
	}
	if m.api == nil {
		return pendingRefreshResults(pendingIDs, ErrMediaRefresh), nil
	}

	message, refreshErr := m.api.Message(ctx, channelID, sourceMessageID)
	if refreshErr != nil {
		var apiErr *APIError
		if errors.As(refreshErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return unrecoverableResults(pendingIDs), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return pendingRefreshResults(
				pendingIDs, fmt.Errorf("%w: %w", ErrMediaRefresh, ctxErr),
			), nil
		}
		return pendingRefreshResults(pendingIDs, ErrMediaRefresh), nil
	}

	fresh := make(map[string]Attachment, len(message.Attachments))
	for _, attachment := range message.Attachments {
		fresh["discord:"+attachment.ID] = attachment
	}
	refs := attachmentRefsInOrder(existing)
	refIndex := make(map[string]int, len(refs))
	for i := range refs {
		refIndex[refs[i].SourceAttachmentID] = i
	}
	matched := false
	for _, sourceAttachmentID := range pendingIDs {
		attachment, ok := fresh[sourceAttachmentID]
		if !ok {
			continue
		}
		refs[refIndex[sourceAttachmentID]] = mapAttachments([]Attachment{attachment})[0]
		matched = true
	}
	// Persist refreshed provenance before using any new signed URL. Missing
	// attachments keep their last observed marker unchanged.
	if matched {
		if err := m.replaceMetadata(messageID, refs); err != nil {
			return MediaResult{}, err
		}
	}

	result := MediaResult{Items: make([]MediaItemResult, 0, len(pendingIDs))}
	for _, sourceAttachmentID := range pendingIDs {
		attachment, ok := fresh[sourceAttachmentID]
		if !ok {
			result.Items = append(result.Items, MediaItemResult{
				SourceAttachmentID: sourceAttachmentID,
				Outcome:            MediaUnrecoverable,
				Err:                ErrMediaUnrecoverable,
			})
			continue
		}
		idx := refIndex[sourceAttachmentID]
		stored, downloadErr := m.downloadAttachment(ctx, attachment, refs[idx])
		if downloadErr != nil {
			result.Items = append(result.Items, MediaItemResult{
				SourceAttachmentID: sourceAttachmentID,
				Outcome:            MediaPending,
				Err:                downloadErr,
			})
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Items = append(result.Items, MediaItemResult{
				SourceAttachmentID: sourceAttachmentID,
				Outcome:            MediaPending,
				Err:                ctxErr,
			})
			continue
		}
		refs[idx] = stored
		if err := m.replaceMetadata(messageID, refs); err != nil {
			result.Items = append(result.Items, MediaItemResult{
				SourceAttachmentID: sourceAttachmentID,
				Outcome:            MediaPending,
				Err:                ErrMediaStorage,
			})
			return result, err
		}
		result.Items = append(result.Items, MediaItemResult{
			SourceAttachmentID: sourceAttachmentID,
			Outcome:            MediaDownloaded,
		})
	}
	return result, nil
}

func pendingRefreshResults(sourceAttachmentIDs []string, err error) MediaResult {
	result := MediaResult{Items: make([]MediaItemResult, 0, len(sourceAttachmentIDs))}
	for _, sourceAttachmentID := range sourceAttachmentIDs {
		result.Items = append(result.Items, MediaItemResult{
			SourceAttachmentID: sourceAttachmentID,
			Outcome:            MediaPending,
			Err:                err,
		})
	}
	return result
}

func unrecoverableResults(sourceAttachmentIDs []string) MediaResult {
	result := MediaResult{Items: make([]MediaItemResult, 0, len(sourceAttachmentIDs))}
	for _, sourceAttachmentID := range sourceAttachmentIDs {
		result.Items = append(result.Items, MediaItemResult{
			SourceAttachmentID: sourceAttachmentID,
			Outcome:            MediaUnrecoverable,
			Err:                ErrMediaUnrecoverable,
		})
	}
	return result
}

func attachmentRefsInOrder(refs map[string]store.AttachmentRef) []store.AttachmentRef {
	ids := make([]string, 0, len(refs))
	for sourceAttachmentID := range refs {
		ids = append(ids, sourceAttachmentID)
	}
	sort.Strings(ids)
	ordered := make([]store.AttachmentRef, 0, len(ids))
	for _, sourceAttachmentID := range ids {
		ordered = append(ordered, refs[sourceAttachmentID])
	}
	return ordered
}

func (m *MediaArchiver) replaceMetadata(messageID int64, refs []store.AttachmentRef) error {
	if err := m.store.ReplaceMessageDiscordAttachments(messageID, refs); err != nil {
		return fmt.Errorf("persist Discord attachment metadata: %w", err)
	}
	if err := m.store.RecomputeMessageAttachmentStats(messageID); err != nil {
		return fmt.Errorf("recompute Discord attachment metadata: %w", err)
	}
	return nil
}

func (m *MediaArchiver) downloadAttachment(
	ctx context.Context, attachment Attachment, marker store.AttachmentRef,
) (store.AttachmentRef, error) {
	if attachment.Size > m.maxBytes {
		return marker, ErrMediaTooLarge
	}
	requestURL, err := m.validateMediaURL(attachment.URL, attachment.ID, attachment.Ephemeral)
	if err != nil {
		return marker, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return marker, ErrInvalidMediaURL
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", UserAgent)

	response, err := m.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return marker, ctxErr
		}
		if errors.Is(err, ErrMediaRedirect) {
			return marker, ErrMediaRedirect
		}
		return marker, ErrMediaDownload
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return marker, fmt.Errorf("%w: HTTP %d", ErrMediaDownload, response.StatusCode)
	}
	if response.ContentLength > m.maxBytes {
		return marker, ErrMediaTooLarge
	}

	temporary, err := os.CreateTemp("", ".msgvault-discord-media-*")
	if err != nil {
		return marker, ErrMediaStorage
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	written, copyErr := io.Copy(temporary, io.LimitReader(response.Body, m.maxBytes+1))
	if copyErr != nil {
		_ = temporary.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return marker, ctxErr
		}
		return marker, ErrMediaDownload
	}
	if written > m.maxBytes {
		_ = temporary.Close()
		return marker, ErrMediaTooLarge
	}
	if err := temporary.Close(); err != nil {
		return marker, ErrMediaStorage
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return marker, ctxErr
	}
	content, err := readMediaFile(ctx, temporaryPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return marker, ctxErr
		}
		return marker, ErrMediaStorage
	}
	mimeAttachment := &mime.Attachment{
		Filename:    attachment.Filename,
		ContentType: attachment.ContentType,
		Content:     content,
	}
	storagePath, err := m.storeAttachmentFile(m.attachmentsDir, mimeAttachment)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return marker, ctxErr
	}
	if err != nil || storagePath == "" {
		return marker, ErrMediaStorage
	}
	marker.StoragePath = storagePath
	marker.ContentHash = mimeAttachment.ContentHash
	marker.Size = len(content)
	return marker, nil
}

type mediaContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *mediaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func readMediaFile(ctx context.Context, filePath string) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var content bytes.Buffer
	if _, err := io.Copy(&content, &mediaContextReader{ctx: ctx, reader: file}); err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

func (m *MediaArchiver) validateMediaURL(raw, expectedAttachmentID string, ephemeral bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, ErrInvalidMediaURL
	}
	origin := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	if _, ok := m.allowedOrigins[origin]; !ok {
		return nil, ErrInvalidMediaURL
	}
	escapedPath := parsed.EscapedPath()
	parts := strings.Split(escapedPath, "/")
	if len(parts) != 5 || parts[0] != "" || parts[4] == "" {
		return nil, ErrInvalidMediaURL
	}
	validPathKind := parts[1] == "attachments" || (ephemeral && parts[1] == "ephemeral-attachments")
	if !validPathKind {
		return nil, ErrInvalidMediaURL
	}
	if _, err := snowflakePathValue("attachment container ID", parts[2]); err != nil {
		return nil, ErrInvalidMediaURL
	}
	if _, err := snowflakePathValue("attachment ID", parts[3]); err != nil {
		return nil, ErrInvalidMediaURL
	}
	if parts[3] != expectedAttachmentID {
		return nil, ErrInvalidMediaURL
	}
	filename, err := url.PathUnescape(parts[4])
	if err != nil || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		return nil, ErrInvalidMediaURL
	}
	return parsed, nil
}
