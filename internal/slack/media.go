package slack

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// defaultMaxMediaBytes caps individual file downloads (config max_media_mb
// overrides). It mirrors the config-layer default exactly.
const defaultMaxMediaBytes = attachmentpolicy.DefaultChatMaxBytes

// mediaHost is the only host file bytes are ever fetched from with the
// bearer token. Message JSON is attacker-influenceable (any workspace member
// authors it), so fetching arbitrary url_private values would hand the token
// to whatever host the URL names — the Slack analogue of the Graph
// token-exfiltration finding on the Teams PR. Off-host files are recorded as
// metadata-only link rows instead.
const mediaHost = "files.slack.com"

// maxMediaRedirects bounds the redirect chain a file download may follow.
// Slack uses a single hop to its signed delivery host; the limit stops a
// redirect loop from becoming an unbounded fetch.
const maxMediaRedirects = 5

// errOffHost reports a file URL that is not on files.slack.com.
var errOffHost = errors.New("file URL not on files.slack.com")

// fileHTTPError preserves the response status so callers can distinguish
// terminal source deletion from a retryable download failure.
type fileHTTPError struct {
	statusCode int
}

func (e *fileHTTPError) Error() string {
	return fmt.Sprintf("slack file GET: status %d", e.statusCode)
}

// slackAttachmentID namespaces Slack-managed attachment rows in
// attachments.source_attachment_id.
func slackAttachmentID(fileID string) string {
	return "slack:" + fileID
}

// mediaTypeOf maps a Slack file's mimetype to msgvault's attachments.media_type.
func mediaTypeOf(f *File) string {
	switch {
	case strings.HasPrefix(f.Mimetype, "image/"):
		return "image"
	case strings.HasPrefix(f.Mimetype, "video/"):
		return "video"
	case strings.HasPrefix(f.Mimetype, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

// fetchableURL validates a file's private URL for token-bearing download:
// https, exactly files.slack.com, no userinfo. Anything else is off-host.
func fetchableURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse file URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, mediaHost) {
		return "", fmt.Errorf("%q: %w", rawURL, errOffHost)
	}
	return u.String(), nil
}

// DownloadFile fetches a files.slack.com URL with the client's credentials,
// capped at maxBytes.
//
// Slack answers a file GET with a 302 to a pre-signed URL on a delivery host
// (slack-files.com), so refusing every redirect leaves those files permanently
// undownloadable. The redirect is followed, but the credentials (the bearer
// token and, for a browser session, the d cookie) are stripped from the hop:
// the signed target authenticates with its own query parameter and needs no
// Authorization header, so the credentials never travel off files.slack.com.
// This keeps the guarantee that motivated the original refusal — the
// credentials reach exactly one host — while letting the download complete.
func (c *Client) DownloadFile(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	fetchURL, err := fetchableURL(rawURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	if err := (Credentials{Token: c.token, Cookie: c.cookie}).Validate(); err != nil {
		return nil, err
	}
	c.authenticate(req)
	// Ask for the stored bytes. Left to itself the transport requests gzip and
	// inflates the body through a 4 KiB reader, which acknowledges every 4 KiB
	// with its own HTTP/2 WINDOW_UPDATE. Slack's delivery host drops the
	// connection after a few thousand of them, so a text file of several
	// megabytes always ended in "unexpected EOF". The redirect hop inherits
	// this header.
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{
		Timeout: attachmentpolicy.DownloadTimeout(maxBytes),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxMediaRedirects {
				return fmt.Errorf(
					"slack file GET: stopped after %d redirects", maxMediaRedirects)
			}
			// A same-host hop keeps the token, and the signed target URL is
			// itself a credential, so neither may travel in cleartext. Check
			// the scheme before the host so no hop downgrades to http.
			if req.URL.Scheme != "https" {
				return fmt.Errorf(
					"slack file GET: refusing non-HTTPS redirect to %s", req.URL.Host)
			}
			// Userinfo on a hop would let the target rewrite the credentials
			// the request carries.
			if req.URL.User != nil {
				return errors.New("slack file GET: refused unsafe redirect")
			}
			// Carrying the token to another host would hand it to whoever
			// controls that host. The signed redirect target does not need it.
			if !strings.EqualFold(req.URL.Host, mediaHost) {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		},
	}
	if c.mediaTransport != nil {
		client.Transport = c.mediaTransport
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &fileHTTPError{statusCode: resp.StatusCode}
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("slack file GET: %d bytes: %w", resp.ContentLength, ErrAssetTooLarge)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("slack file GET: read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrAssetTooLarge
	}
	if len(data) == 0 {
		return nil, errors.New("slack file GET: empty response body")
	}
	return data, nil
}

// persistFiles downloads a message's files into content-addressed storage
// and replaces the message's Slack attachment rows. Already-downloaded media
// (matched by source_attachment_id) is kept without re-fetching — including
// files the source has since tombstoned or dropped from the message
// (archive semantics: source deletions never propagate). External or
// off-host files become metadata-only link rows (media_type "link", the
// permalink as storage path). Failed downloads remain retryable, while
// deliberate exclusions retain typed markers until current policy allows them.
//
// DOWNLOAD failures are non-fatal because they leave that durable marker;
// STORE failures are fatal and hold the cursor — a row write that fails
// leaves downloaded bytes orphaned in CAS with no marker, invisible to
// backfill forever, so the run must stop rather than advance past it.
func (imp *Importer) persistFiles(ctx context.Context, syncID, messageID int64, m *Message, opts ImportOptions, sum *ImportSummary) error {
	existing, err := imp.store.MessageSlackAttachmentsContext(ctx, messageID)
	if err != nil {
		return fmt.Errorf("read attachment rows: %w", err)
	}
	if len(m.Files) == 0 && len(existing) == 0 {
		return nil
	}
	policy, maxBytes := mediaLimits(opts)
	refs := make([]store.AttachmentRef, 0, len(m.Files))
	seen := map[string]bool{}
	for i := range m.Files {
		f := &m.Files[i]
		if f.ID == "" || f.Mode == "tombstone" {
			continue // preserved below if a downloaded row already exists
		}
		sourceAttID := slackAttachmentID(f.ID)
		seen[sourceAttID] = true
		if prev, ok := existing[sourceAttID]; ok && prev.ContentHash != "" {
			prev.Role = store.AttachmentRoleStandalone
			prev.RoleSource = store.AttachmentRoleSourceProviderExplicit
			refs = append(refs, prev)
			continue
		}
		fetchRef := f.URLPrivate
		if fetchRef == "" {
			fetchRef = f.URLPrivateDownload
		}
		// The store write silently skips rows with an empty storage path, so
		// a file Slack serves without a permalink would lose its marker (and
		// with it any chance of backfill) with no error. Fall back to the
		// fetch URL, then to a deterministic sentinel: the row must exist.
		storageRef := f.Permalink
		if storageRef == "" {
			storageRef = fetchRef
		}
		if storageRef == "" {
			storageRef = "slack:pending:" + f.ID
		}
		linkRow := store.AttachmentRef{
			Filename:           f.Name,
			MimeType:           f.Mimetype,
			StoragePath:        storageRef,
			Size:               int(f.Size),
			SourceAttachmentID: sourceAttID,
			MediaType:          "link",
		}
		pendingRow := linkRow
		pendingRow.MediaType = ""
		pendingRow.State = attachmentpolicy.StatePending
		if previous, ok := existing[sourceAttID]; ok && previous.Size > pendingRow.Size {
			pendingRow.Size = previous.Size
		}

		// pend leaves a retryable marker; link records metadata permanently.
		pend := func(status, kind string, err error) {
			if status == store.SyncRunItemStatusError && err != nil {
				if urlErr, ok := errors.AsType[*url.Error](err); ok {
					err = urlErr.Err
				}
				reason := strings.ToValidUTF8(err.Error(), "\ufffd")
				metadata, _ := json.Marshal(map[string]string{"fetch_error": reason})
				pendingRow.Metadata = string(metadata)
				if sum.FirstMediaError == "" {
					sum.FirstMediaError = textutil.TruncateRunes(f.ID+": "+reason, 200)
				}
				err = fmt.Errorf("%s: %s", f.ID, reason)
			}
			imp.recordItem(ctx, syncID, sourceMessageID("", m.TS), "attachment", status, kind, err)
			if status == store.SyncRunItemStatusSkipped {
				pendingRow.State = attachmentpolicy.StateSkipped
				pendingRow.SkipReason = attachmentpolicy.SkipSizeCap
			} else {
				pendingRow.State = attachmentpolicy.StateFailed
				pendingRow.SkipReason = attachmentpolicy.SkipFetchFailure
			}
			refs = append(refs, pendingRow)
			if pendingRow.State == attachmentpolicy.StateSkipped {
				sum.AttachmentsSkipped++
			} else {
				sum.AttachmentsPending++
			}
			if status == store.SyncRunItemStatusError {
				sum.Errors++
			}
		}
		if _, herr := fetchableURL(fetchRef); herr != nil || f.IsExternal {
			// Off-host / external: never fetched with the token (see mediaHost).
			refs = append(refs, linkRow)
			continue
		}
		if opts.NoMedia || opts.AttachmentsDir == "" {
			// Deferred, not declined: a pending marker keeps the file
			// discoverable by backfill-slack-media once media is enabled.
			// A link row here would hide it from the pending queries
			// forever (link rows are reserved for genuinely external
			// files that must never be fetched with the token).
			pendingRow.Metadata = existing[sourceAttID].Metadata
			refs = append(refs, pendingRow)
			sum.AttachmentsPending++
			continue
		}
		if reason := policy.Evaluate(opts.MediaConversation, int64(pendingRow.Size)); reason != "" {
			if reason == attachmentpolicy.SkipSizeCap {
				pendingRow.SkipReason = reason
				pend(store.SyncRunItemStatusSkipped, "slack_media_too_large",
					fmt.Errorf("file %s is %d bytes (cap %d)", f.ID, f.Size, maxBytes))
				continue
			}
			pendingRow.State = attachmentpolicy.StateSkipped
			pendingRow.SkipReason = reason
			imp.recordItem(ctx, syncID, sourceMessageID("", m.TS), "attachment",
				store.SyncRunItemStatusSkipped, "slack_media_policy", nil)
			refs = append(refs, pendingRow)
			sum.AttachmentsSkipped++
			continue
		}
		if f.Size > 0 && f.Size > maxBytes {
			pend(store.SyncRunItemStatusSkipped, "slack_media_too_large",
				fmt.Errorf("file %s is %d bytes (cap %d)", f.ID, f.Size, maxBytes))
			continue
		}
		got := imp.fetchFile(ctx, f, sourceAttID, fetchRef, maxBytes, opts.AttachmentsDir)
		derr := got.err
		if errors.Is(derr, ErrAssetTooLarge) {
			pendingRow.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, int64(pendingRow.Size))
			pend(store.SyncRunItemStatusSkipped, "slack_media_too_large", derr)
			continue
		}
		var httpErr *fileHTTPError
		if errors.As(derr, &httpErr) &&
			(httpErr.statusCode == http.StatusNotFound || httpErr.statusCode == http.StatusGone) {
			// Slack can keep file metadata after the backing bytes have been
			// deleted. Preserve the permalink and file provenance as a
			// metadata-only link, but terminate retry debt that can never pay.
			imp.recordItem(ctx, syncID, sourceMessageID("", m.TS), "attachment",
				store.SyncRunItemStatusSkipped, "slack_media_gone", derr)
			refs = append(refs, linkRow)
			continue
		}
		if derr != nil {
			if ctx.Err() != nil {
				// Interrupted: keep the pending marker without charging an error.
				refs = append(refs, pendingRow)
				sum.AttachmentsPending++
				continue
			}
			pend(store.SyncRunItemStatusError, "slack_media_error", derr)
			continue
		}
		if got.storeErr != nil || got.storagePath == "" {
			pend(store.SyncRunItemStatusError, "slack_media_error", got.storeErr)
			continue
		}
		stored := store.AttachmentRef{
			Filename:           f.Name,
			MimeType:           f.Mimetype,
			StoragePath:        got.storagePath,
			ContentHash:        got.contentHash,
			Size:               got.size,
			SourceAttachmentID: sourceAttID,
			MediaType:          mediaTypeOf(f),
			Role:               store.AttachmentRoleStandalone,
			RoleSource:         store.AttachmentRoleSourceProviderExplicit,
			State:              attachmentpolicy.StateStored,
		}
		refs = append(refs, stored)
		sum.AttachmentsDownloaded++
	}
	// Files tombstoned or omitted at the source keep their archived rows.
	// Downloaded media and existing metadata-only links survive unchanged;
	// pending markers become terminal metadata-only links. The conversion
	// preserves the last captured filename, URL, and size without wedging a
	// retry queue that can no longer fetch the file.
	var keep []string
	for id := range existing {
		if !seen[id] {
			keep = append(keep, id)
		}
	}
	sort.Strings(keep)
	for _, id := range keep {
		ref := existing[id]
		if ref.ContentHash == "" && ref.MediaType != "link" {
			ref.MediaType = "link"
		}
		refs = append(refs, ref)
	}
	if err := imp.store.ReplaceMessageSlackAttachmentsContext(ctx, messageID, refs); err != nil {
		return fmt.Errorf("replace attachment rows: %w", err)
	}
	if err := imp.store.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
		return fmt.Errorf("recompute attachment stats: %w", err)
	}
	return nil
}

// mediaLimits returns the run's media policy and per-file byte cap.
func mediaLimits(opts ImportOptions) (attachmentpolicy.Policy, int64) {
	maxBytes := opts.MaxMediaBytes
	if opts.MediaPolicy.MaxBytes > 0 {
		maxBytes = opts.MediaPolicy.MaxBytes
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	policy := opts.MediaPolicy
	policy.MaxBytes = maxBytes
	return policy, maxBytes
}

// fetchedFile is one file download and its content-addressed store result.
type fetchedFile struct {
	storagePath string
	contentHash string
	size        int
	err         error // download failure
	storeErr    error // local store failure
}

// fetchFile downloads f and stores it in content-addressed storage, or
// returns the result preparePage already produced for it. The result stays
// for the rest of the page, since several messages can share one file.
func (imp *Importer) fetchFile(ctx context.Context, f *File, sourceAttID, fetchRef string, maxBytes int64, dir string) fetchedFile {
	if got, ok := imp.fetched[sourceAttID]; ok {
		return got
	}
	data, err := imp.client.DownloadFile(ctx, fetchRef, maxBytes)
	if err != nil {
		return fetchedFile{err: err}
	}
	ma := &mime.Attachment{Filename: f.Name, ContentType: f.Mimetype, Content: data}
	storagePath, err := export.StoreAttachmentFile(dir, ma)
	return fetchedFile{storagePath: storagePath, contentHash: ma.ContentHash, size: len(data), storeErr: err}
}

// fetchPageFiles downloads, before the page's write transaction, every file
// persistFiles would download for the page's messages. It applies the same
// checks persistFiles makes before its download, in the same order, so it
// fetches no file that persistFiles would not; a file it misses is fetched
// by persistFiles as before.
func (imp *Importer) fetchPageFiles(ctx context.Context, cc *convScope, msgs []Message) error {
	opts := cc.opts
	if opts.NoMedia || opts.AttachmentsDir == "" {
		return nil
	}
	var ids []string
	for i := range msgs {
		if len(msgs[i].Files) > 0 && msgs[i].Type == "message" && msgs[i].TS != "" {
			ids = append(ids, sourceMessageID(cc.channelID, msgs[i].TS))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	messageIDs, err := imp.store.MessageExistsBatchContext(ctx, cc.sourceID, ids)
	if err != nil {
		return fmt.Errorf("check existing Slack messages: %w", err)
	}
	policy, maxBytes := mediaLimits(opts)
	if imp.fetched == nil {
		imp.fetched = make(map[string]fetchedFile)
	}
	for i := range msgs {
		m := &msgs[i]
		if len(m.Files) == 0 || m.Type != "message" || m.TS == "" {
			continue
		}
		var existing map[string]store.AttachmentRef
		if id, ok := messageIDs[sourceMessageID(cc.channelID, m.TS)]; ok {
			if existing, err = imp.store.MessageSlackAttachmentsContext(ctx, id); err != nil {
				return fmt.Errorf("read attachment rows: %w", err)
			}
		}
		for j := range m.Files {
			f := &m.Files[j]
			if f.ID == "" || f.Mode == "tombstone" {
				continue
			}
			sourceAttID := slackAttachmentID(f.ID)
			prev, ok := existing[sourceAttID]
			if ok && prev.ContentHash != "" {
				continue
			}
			fetchRef := f.URLPrivate
			if fetchRef == "" {
				fetchRef = f.URLPrivateDownload
			}
			if _, herr := fetchableURL(fetchRef); herr != nil || f.IsExternal {
				continue
			}
			size := int(f.Size)
			if ok && prev.Size > size {
				size = prev.Size
			}
			if policy.Evaluate(opts.MediaConversation, int64(size)) != "" {
				continue
			}
			if f.Size > 0 && f.Size > maxBytes {
				continue
			}
			if _, done := imp.fetched[sourceAttID]; done {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			imp.fetched[sourceAttID] = imp.fetchFile(ctx, f, sourceAttID, fetchRef, maxBytes, opts.AttachmentsDir)
		}
	}
	return nil
}
