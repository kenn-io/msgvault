// Package attachmentverify checks retained attachment content and thumbnails
// against live SQL references using the existing loose and packed readers.
// It reports observations during the run and never repairs storage.
package attachmentverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const (
	// Verified means the stream reached verified EOF, closed successfully,
	// and matched the occurrence size when one was recorded.
	Verified = "verified"
	// MissingLoose means no loose candidate could be opened and the catalogue
	// still referenced the content without assigning it to a pack.
	MissingLoose = "missing_loose"
	// PackUnavailable means the authoritative pack could not be opened.
	PackUnavailable = "pack_unavailable"
	// Corrupt means the physical reader detected a content or container mismatch.
	Corrupt = "corrupt"
	// MetadataMismatch means verified bytes disagree with the occurrence size.
	MetadataMismatch = "metadata_mismatch"
	// Omitted means an explicit policy skip records why no bytes were retained.
	Omitted = "intentionally_omitted"
	// NotMaterialized means the reference makes no claim to local retained bytes.
	// Pending or failed acquisition alone does not establish intentional omission.
	NotMaterialized = "not_materialized"
	// Inconclusive means metadata, storage errors, or concurrent changes prevent
	// a conclusion about integrity. It does not establish missing or corrupt bytes.
	Inconclusive = "inconclusive"
	// NotChecked means a limit or interruption left verification unfinished.
	// Its attachment row must remain eligible when the caller resumes.
	NotChecked = "not_checked"
)

// Options bounds selection, physical reads and runtime. The page counts
// attachment rows; each has one content slot and an optional thumbnail slot.
type Options struct {
	AfterID  int64
	Limit    int
	MaxBytes int64
	Timeout  time.Duration
}

// Scope records the requested bounds using transport-friendly values.
type Scope struct {
	AfterID  int64  `json:"after_id"`
	Limit    int    `json:"limit"`
	MaxBytes int64  `json:"max_bytes"`
	Timeout  string `json:"timeout"`
}

// Validate rejects bounds that would allow unbounded selection, I/O, or runtime.
func (o Options) Validate() error {
	if o.AfterID < 0 || o.Limit < 1 || o.Limit > 1000 {
		return errors.New("after-id must be nonnegative and limit must be between 1 and 1000")
	}
	if o.MaxBytes < 1 || o.MaxBytes > 1<<40 {
		return errors.New("max-bytes must be between 1 and 1099511627776")
	}
	if o.Timeout < time.Second || o.Timeout > time.Hour {
		return errors.New("timeout must be between 1s and 1h")
	}
	return nil
}

// StreamStore opens content through the daemon's shared loose and packed reader.
// Integrity errors may arrive on Read or Close; opening a stream is not verification.
type StreamStore interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

// Reference reports one attachment content or thumbnail slot. References sharing
// a hash reuse the same physical read, but each retains its own size comparison.
type Reference struct {
	AttachmentID int64  `json:"attachment_id"`
	MessageID    int64  `json:"message_id"`
	Kind         string `json:"kind"`
	Hash         string `json:"hash,omitempty"`
	ExpectedSize *int64 `json:"expected_size,omitempty"`
	BytesRead    int64  `json:"bytes_read"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	PackID       string `json:"pack_id,omitempty"`
}

// Report describes one bounded page. State describes traversal, not integrity:
// a complete page may contain faults, which Counts and ReferencesAffected expose.
type Report struct {
	Scope        Scope     `json:"scope"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	State        string    `json:"state"`
	RowsSelected int       `json:"rows_selected"`
	// NextAfterID advances only past rows whose slots have all been checked
	// or classified. A completed inconclusive result can therefore be passed.
	NextAfterID int64 `json:"next_after_id"`
	// More reports rows beyond the selected page, not unfinished work in it.
	More               bool `json:"more"`
	ReferencesSelected int  `json:"references_selected"`
	DistinctContents   int  `json:"distinct_contents"`
	ContentsOpened     int  `json:"contents_opened"`
	// ContentsVerified counts hashes that passed the physical reader. Individual
	// references may still have a MetadataMismatch against their recorded size.
	ContentsVerified   int `json:"contents_verified"`
	ReferencesVerified int `json:"references_verified"`
	ReferencesAffected int `json:"references_affected"`
	// BytesRead counts decoded bytes once per hash, unlike Reference.BytesRead.
	BytesRead  int64          `json:"bytes_read"`
	Counts     map[string]int `json:"counts"`
	StopReason string         `json:"stop_reason,omitempty"`
	References []Reference    `json:"references"`
}

type selectedContent struct {
	hash             string
	recordedPaths    []string
	referenceIndexes []int
}

// Run selects once, releases SQL resources, then consumes and closes each
// distinct content stream. The caller must supply the daemon's shared store
// so pack retirement uses its existing reader leases. A report is not a global
// SQL/filesystem snapshot. Concurrent authority changes are inconclusive.
func Run(ctx context.Context, st *store.Store, blobs StreamStore, attachmentsDir string, opts Options) (Report, error) {
	report := Report{Scope: Scope{AfterID: opts.AfterID, Limit: opts.Limit, MaxBytes: opts.MaxBytes, Timeout: opts.Timeout.String()}, StartedAt: time.Now().UTC(), State: "inconclusive", NextAfterID: opts.AfterID,
		Counts: map[string]int{}, References: []Reference{}}
	if err := opts.Validate(); err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	rows, more, err := st.SelectAttachmentVerificationRows(ctx, opts.AfterID, opts.Limit)
	if err != nil {
		return report, err
	}
	report.RowsSelected, report.More = len(rows), more
	var contents []*selectedContent
	byHash := map[string]*selectedContent{}
	add := func(row store.AttachmentVerificationRow, kind, hash, path string, size *int64) {
		ref := Reference{AttachmentID: row.ID, MessageID: row.MessageID, Kind: kind,
			Hash: strings.ToLower(hash), ExpectedSize: size, Status: NotChecked}
		switch {
		case len(path) > 4096:
			ref.Status, ref.Reason = Inconclusive, "recorded path exceeds 4096 bytes"
		case hash == "":
			ref.Status, ref.Reason = NotMaterialized, "no retained content hash; bytes not verified"
			// A stale skip must not hide a local file claim. Only a recognized policy
			// reason without a retained hash or local path establishes omission.
			if kind == "content" && (path == "" || nonLocalLocator(path)) && row.State == string(attachmentpolicy.StateSkipped) && policySkip(row.SkipReason) {
				ref.Status, ref.Reason = Omitted, "recorded policy omission"
			}
			if path != "" && !nonLocalLocator(path) {
				ref.Status, ref.Reason = Inconclusive, "local path has no expected content hash"
			}
		case export.ValidateContentHash(hash) != nil:
			ref.Status, ref.Reason = Inconclusive, "invalid expected SHA-256"
		case nonLocalLocator(path):
			// Providers also use storage_path for remote IDs and pending markers.
			// Even with a hash, these values do not claim a local file exists.
			ref.Status, ref.Reason = NotMaterialized, "external locator; no local storage claim"
		}
		index := len(report.References)
		report.References = append(report.References, ref)
		if ref.Status != NotChecked {
			return
		}
		item := byHash[ref.Hash]
		if item == nil {
			item = &selectedContent{hash: ref.Hash}
			byHash[ref.Hash] = item
			contents = append(contents, item)
		}
		item.referenceIndexes = append(item.referenceIndexes, index)
		if path != "" {
			item.recordedPaths = append(item.recordedPaths, path)
		}
	}
	for _, row := range rows {
		var size *int64
		if row.Size.Valid {
			value := row.Size.Int64
			size = &value
		}
		add(row, "content", row.ContentHash, row.StoragePath, size)
		if row.ThumbnailHash != "" || row.ThumbnailPath != "" {
			add(row, "thumbnail", row.ThumbnailHash, row.ThumbnailPath, nil)
		}
	}
	report.ReferencesSelected, report.DistinctContents = len(report.References), len(contents)
	for _, item := range contents {
		if ctx.Err() != nil {
			report.StopReason = "cancelled or timeout"
			break
		}
		if report.BytesRead >= opts.MaxBytes {
			report.StopReason = "byte limit"
			break
		}
		status, reason, packID, bytesRead, stopReason, opened := verifyContent(ctx, st, blobs, attachmentsDir, item, opts.MaxBytes-report.BytesRead)
		report.BytesRead += bytesRead
		if opened {
			report.ContentsOpened++
		}
		if status == Verified {
			report.ContentsVerified++
		}
		for _, index := range item.referenceIndexes {
			ref := &report.References[index]
			ref.Status, ref.Reason, ref.PackID, ref.BytesRead = status, reason, packID, bytesRead
			if status == Verified && ref.ExpectedSize != nil && *ref.ExpectedSize != bytesRead {
				ref.Status, ref.Reason = MetadataMismatch, "verified content length differs from recorded occurrence size"
			}
		}
		if stopReason != "" {
			report.StopReason = stopReason
			break
		}
	}
	report.State = "complete"
	for _, ref := range report.References {
		report.Counts[ref.Status]++
		switch ref.Status {
		case Verified:
			report.ReferencesVerified++
		case MissingLoose, PackUnavailable, Corrupt, MetadataMismatch:
			report.ReferencesAffected++
		case Inconclusive, NotMaterialized, NotChecked:
			report.State = "inconclusive"
		}
	}
	if report.More || report.StopReason != "" {
		report.State = "partial"
	}
	// Hash deduplication can finish later rows before an earlier row's thumbnail.
	// Resume from the first unfinished row so no slot is skipped, even if that
	// repeats successful reads from this page.
	unfinished := make(map[int64]bool)
	for _, ref := range report.References {
		if ref.Status == NotChecked {
			unfinished[ref.AttachmentID] = true
		}
	}
	for _, row := range rows {
		if unfinished[row.ID] {
			break
		}
		report.NextAfterID = row.ID
	}
	report.FinishedAt = time.Now().UTC()
	return report, nil
}

// nonLocalLocator recognizes current importer placeholders and exclusion
// markers. Other paths still need an expected hash to verify retained bytes.
func nonLocalLocator(path string) bool {
	if store.IsURLStoragePath(path) {
		return true
	}
	path = strings.ToLower(path)
	for _, prefix := range []string{
		"mxc://", "matrix:mxc://", "inline:pending:", "discord:pending:",
		"slack:pending:", "slackdump:skipped:", "slackdump:missing:",
		"slackdump:available:", "excluded:", "chatwoot:attachment:",
		"chatwoot:recording:", "chatwoot:part:", "imessage:",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// SQL drivers can report interruption before the caller observes ctx.Err().
// Treat interrupted lookups and rechecks as unfinished, even after verified EOF,
// so cancellation cannot advance the resume cursor past an unchecked reference.
func interrupted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func policySkip(reason string) bool {
	switch attachmentpolicy.SkipReason(reason) {
	case attachmentpolicy.SkipPolicyScope, attachmentpolicy.SkipParticipantThreshold, attachmentpolicy.SkipAccountPolicy, attachmentpolicy.SkipSizeCap:
		return true
	default:
		return false
	}
}

func sameAuthority(a, b store.AttachmentBlobLocation) bool {
	if a.Referenced != b.Referenced || (a.Pack == nil) != (b.Pack == nil) {
		return false
	}
	return a.Pack == nil || *a.Pack == *b.Pack
}

// verifyContent consumes one distinct hash and rechecks its catalogue membership
// and pack assignment. These checks detect observed authority changes; they do
// not freeze occurrence metadata or rule out changes between observations.
func verifyContent(ctx context.Context, st *store.Store, blobs StreamStore, attachmentsDir string, item *selectedContent, remaining int64) (status, reason, packID string, bytesRead int64, stopReason string, opened bool) {
	before, err := st.ResolveAttachmentBlobContext(ctx, item.hash)
	if interrupted(ctx, err) {
		return NotChecked, "cancelled or timeout before verification completed", "", 0, "cancelled or timeout", false
	}
	if err != nil {
		return Inconclusive, "catalogue could not be resolved", "", 0, "", false
	}
	if !before.Referenced {
		return Inconclusive, "reference is no longer live", "", 0, "", false
	}
	if before.Pack != nil {
		packID = before.Pack.PackID
	}
	reader, _, err := blobs.OpenStream(ctx, item.hash)
	if errors.Is(err, os.ErrNotExist) && before.Pack == nil {
		// Recheck authority before trying a legacy path. Never use loose fallback
		// to hide a missing authoritative pack. The Kit reader handles migrations
		// of canonical loose files and packs itself.
		current, resolveErr := st.ResolveAttachmentBlobContext(ctx, item.hash)
		if interrupted(ctx, resolveErr) {
			return NotChecked, "cancelled or timeout before verification completed", packID, 0, "cancelled or timeout", false
		}
		if resolveErr != nil || !sameAuthority(before, current) {
			return Inconclusive, "authority changed or became unavailable", packID, 0, "", false
		}
		var source io.ReadCloser
		source, _, _, _, err = attachmentstore.OpenLooseCandidates(attachmentsDir, item.hash, func() ([]string, error) { return item.recordedPaths, nil })
		if err == nil {
			reader, err = contentverify.NewReadCloser(source, item.hash)
			if err != nil {
				err = errors.Join(err, source.Close())
			}
		}
	}
	// ErrNotExist proves absence only when opening fails and the catalogue
	// remains unchanged. A late Read or Close error cannot establish absence;
	// retain it as an inconclusive result unless the reader proves corruption.
	openErr := err
	opened = openErr == nil
	if opened {
		// Read one byte beyond the remaining budget to distinguish a stream that
		// ends exactly at the limit from one that still has unread content. Always
		// close: integrity errors can surface there after a successful final Read.
		var readErr error
		bytesRead, readErr = io.CopyBuffer(io.Discard, io.LimitReader(contextReader{ctx, reader}, remaining+1), make([]byte, 64<<10))
		closeErr := reader.Close()
		err = errors.Join(readErr, closeErr)
		if bytesRead > remaining {
			return NotChecked, "stream did not reach verified EOF within byte limit", packID, bytesRead, "byte limit", opened
		}
	}
	if ctx.Err() != nil {
		return NotChecked, "cancelled or timeout before verification completed", packID, bytesRead, "cancelled or timeout", opened
	}
	after, resolveErr := st.ResolveAttachmentBlobContext(ctx, item.hash)
	if interrupted(ctx, resolveErr) {
		return NotChecked, "cancelled or timeout before verification completed", packID, bytesRead, "cancelled or timeout", opened
	}
	if resolveErr != nil || !sameAuthority(before, after) {
		return Inconclusive, "authority changed or became unavailable", packID, bytesRead, "", opened
	}
	if err == nil {
		return Verified, "", packID, bytesRead, "", opened
	}
	switch {
	case errors.Is(packstore.ClassifyIntegrityError(err), packstore.ErrPhysicalCorrupt), errors.Is(err, contentverify.ErrMismatch):
		return Corrupt, "physical reader rejected content or container integrity", packID, bytesRead, "", opened
	case errors.Is(openErr, os.ErrNotExist):
		if before.Pack != nil {
			return PackUnavailable, "authoritative pack could not be opened", packID, bytesRead, "", opened
		}
		return MissingLoose, "canonical and selected recorded loose paths are missing", "", bytesRead, "", opened
	default:
		// Permissions, generic I/O, limits and close failures do not prove corruption.
		return Inconclusive, boundedError(err), packID, bytesRead, "", opened
	}
}

// contextReader checks cancellation between reads. It cannot interrupt an
// underlying filesystem call that is already blocked.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func boundedError(err error) string {
	const maxReasonBytes = 512
	// Storage errors can contain arbitrary path bytes. Keep the reason valid
	// UTF-8 so JSON encoding cannot discard the report and its resume cursor.
	message := strings.ToValidUTF8(fmt.Sprintf("read or close could not establish integrity: %v", err), "\uFFFD")
	if len(message) <= maxReasonBytes {
		return message
	}
	end := maxReasonBytes
	// Preserve the byte budget without cutting through a multibyte character.
	for !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end]
}
