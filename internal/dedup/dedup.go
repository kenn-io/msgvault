// Package dedup provides duplicate detection and merging for msgvault.
//
// # Terminology
//
// "Account" means one ingest source/archive (a single Gmail OAuth
// connection, one mbox import, one IMAP source, etc.). "Collection"
// means a named, user-defined grouping of accounts. Cross-source dedup
// is only available via --collection; --account always operates on a
// single source.
//
// # Scoping rules
//
// Without explicit scope, dedup operates on one account at a time and
// duplicate groups can only contain messages ingested twice into the
// same account (for example, re-importing the same mbox twice).
//
// With --account, dedup is restricted to the named account and behaves
// the same way — source boundaries are never crossed.
//
// With --collection, dedup compares messages across every account in
// the collection. This is the only way to merge duplicates that span
// sources, and it is an explicit user opt-in. Pruned losers are hidden
// locally and reversible via --undo. Remote-deletion staging stays
// same-source-only even under collection scope, so the user's
// authoritative remote mailbox can never be touched because of a
// duplicate found in a different account.
//
// Outside collection scope, dedup never merges messages across
// different accounts. This is critical for sent messages: a message
// alice sends to bob is one logical message but it has a legitimate
// copy in alice's Sent folder and another in bob's Inbox. Both copies
// share the same RFC822 Message-ID. If both accounts are archived in
// msgvault, they must be preserved independently because deleting one
// would change the other user's view of history. Sent-message handling
// is covered in more detail by FormatMethodology.
package dedup

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/textproto"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wesm/msgvault/internal/deletion"
	"github.com/wesm/msgvault/internal/store"
)

// Config controls the dedup engine behaviour.
type Config struct {
	// SourcePreference orders source types when picking a survivor
	// inside a duplicate group. Earlier entries win.
	SourcePreference []string

	// DryRun reports what would happen without mutating the database
	// or writing deletion manifests.
	DryRun bool

	// ContentHashFallback also groups messages by normalized raw MIME
	// content after the RFC822 Message-ID pass. This is slower, but can
	// catch duplicates where Message-ID is missing or transport headers
	// are the only difference between copies.
	ContentHashFallback bool

	// AccountSourceIDs restricts dedup to the listed sources and
	// allows cross-source grouping between them. Callers that want
	// strict per-source dedup should leave this empty.
	AccountSourceIDs []int64

	// Account is the canonical identifier for the scoped account
	// (for example, "alice@gmail.com"). It is used when building
	// deletion manifests and in the methodology output.
	Account string

	// ScopeIsCollection is true when AccountSourceIDs spans multiple
	// distinct accounts via --collection. The methodology output
	// branches on this: collection mode intentionally crosses account
	// boundaries, while account/per-source modes do not.
	ScopeIsCollection bool

	// DeleteDupsFromSourceServer, when true, writes pending
	// deletion manifests for pruned duplicates that meet ALL of:
	//   1. the pruned copy lives in a remote source whose type
	//      appears in remoteSourceTypes (gmail today; imap is gated
	//      until staged manifests can be routed to an IMAP executor),
	//   2. the surviving copy is in the SAME source_id (i.e. the
	//      very same remote mailbox holds the winner).
	//
	// This second rule is load-bearing: it guarantees that a
	// merged-pile dedup run can never cause deletions from the
	// user's authoritative Gmail/IMAP account just because a
	// duplicate was found in a local archive. Only true
	// intra-mailbox duplicates are ever proposed for remote
	// deletion.
	//
	// Even with this rule, the field defaults to false so that
	// destructive side effects never happen without an explicit
	// --delete-dups-from-source-server opt-in at the CLI layer.
	DeleteDupsFromSourceServer bool

	// DeletionsDir is the directory where staged deletion manifests
	// are written. Required when DeleteDupsFromSourceServer is true.
	DeletionsDir string

	// IdentityAddressesBySource maps each source ID to the set of
	// confirmed "me" addresses for that source. When a pruned
	// candidate's From: matches the address set for its source,
	// the survivor-selection rule treats the message as a sent
	// copy — in addition to the existing Gmail SENT label and
	// messages.is_from_me signals. Per-source keying ensures that
	// an address confirmed for one account is not treated as "me"
	// for a different account.
	IdentityAddressesBySource map[int64]map[string]struct{}
}

// DefaultSourcePreference is the default source-type authority order.
var DefaultSourcePreference = []string{
	"gmail", "imap", "mbox", "emlx", "hey",
}

// remoteSourceTypes lists source types whose messages can be deleted
// via the deletion-staging machinery.
//
// Only gmail is listed today: the staged-deletion manifest format and
// executor are Gmail-specific (manifest.GmailIDs, gmail.API client). Adding
// "imap" here would let an IMAP dedup run with --delete-dups-from-source-server
// stage manifests that delete-staged would then try to execute through Gmail.
// Re-add IMAP only after manifests record source type and delete-staged can
// route to an IMAP executor.
var remoteSourceTypes = map[string]bool{
	"gmail": true,
}

// Engine orchestrates duplicate detection and merging.
type Engine struct {
	store  *store.Store
	config Config
	logger *slog.Logger
}

// NewEngine creates a new dedup engine.
func NewEngine(st *store.Store, cfg Config, logger *slog.Logger) *Engine {
	if len(cfg.SourcePreference) == 0 {
		cfg.SourcePreference = DefaultSourcePreference
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{store: st, config: cfg, logger: logger}
}

// DuplicateGroup represents a set of messages that are duplicates of
// each other (share the same RFC822 Message-ID in the scoped sources).
type DuplicateGroup struct {
	Key      string             // RFC822 Message-ID or normalized hash
	KeyType  string             // "message-id" or "normalized-hash"
	Messages []DuplicateMessage // all messages in the group
	Survivor int                // index into Messages of the chosen survivor
}

// DuplicateMessage holds metadata for a single message in a duplicate
// group, including sent-message signals for safety checks.
type DuplicateMessage struct {
	ID               int64
	SourceID         int64
	SourceType       string
	SourceIdentifier string
	SourceMessageID  string
	Subject          string
	SentAt           time.Time
	HasRawMIME       bool
	LabelCount       int
	ArchivedAt       time.Time
	IsFromMe         bool
	HasSentLabel     bool
	FromEmail        string
	MatchedIdentity  bool
}

// IsSentCopy reports whether this message appears to be the sender-side
// copy of an outbound email. Three independent signals (OR-combined):
//   - Gmail SENT system label on the message
//   - messages.is_from_me set at ingest time
//   - From: address matches a configured identity address
func (m DuplicateMessage) IsSentCopy() bool {
	return m.HasSentLabel || m.IsFromMe || m.MatchedIdentity
}

// Report summarises the results of a dedup scan.
type Report struct {
	TotalMessages              int64
	DuplicateGroups            int
	DuplicateMessages          int            // messages that would be pruned
	BySourcePair               map[string]int // "gmail+mbox" -> groups
	SampleGroups               []DuplicateGroup
	Groups                     []DuplicateGroup
	BackfilledCount            int64
	ContentHashGroups          int
	SkippedDecompressionErrors int
}

// ExecutionSummary summarises the results of dedup execution.
type ExecutionSummary struct {
	GroupsMerged      int
	MessagesRemoved   int
	LabelsTransferred int
	RawMIMEBackfilled int
	BatchID           string
	StagedManifests   []StagedManifest
}

// StagedManifest records a single deletion manifest created by dedup.
type StagedManifest struct {
	Account      string
	SourceType   string
	ManifestID   string
	MessageCount int
}

// remoteKey groups remote source IDs by the (account, source_type) pair so
// that a user with multiple remote sources sharing the same account
// identifier (e.g. gmail + imap for the same address) gets one manifest per
// source type rather than a single manifest whose SourceType label reflects
// only the first contributor.
type remoteKey struct {
	Account    string
	SourceType string
}

// Scan finds all duplicate groups that dedup would prune.
// AccountSourceIDs must be non-empty to prevent accidental cross-account
// grouping; the CLI ensures this by iterating sources one at a time when
// no explicit --account is given.
//
// Side effect (non-dry-run only): if the scoped sources contain messages
// with no rfc822_message_id but with stored MIME, Scan calls
// store.BackfillRFC822IDs to derive the column from the stored headers
// before grouping. The backfill is idempotent metadata derivation — it
// fills a previously-NULL column from data already on the row, never
// overwrites a non-NULL value, and changes no message content. It happens
// during scan (rather than during merge) because the duplicate groups it
// surfaces are needed for the report the user is shown before the merge
// confirmation. The dedup-batch backup-before-merge contract still holds:
// the backup is sized for the merge (which soft-deletes losers and
// transfers labels), not for this metadata catch-up. Dry-run mode skips
// the backfill and reports the count as a "would-backfill" preview.
func (e *Engine) Scan(ctx context.Context) (*Report, error) {
	if len(e.config.AccountSourceIDs) == 0 {
		return nil, fmt.Errorf("AccountSourceIDs must be non-empty; use per-source iteration for unscoped dedup")
	}

	started := time.Now()
	e.logger.Info("dedup scan start",
		"account", e.config.Account,
		"sources", len(e.config.AccountSourceIDs),
		"is_collection", e.config.ScopeIsCollection,
		"content_hash_fallback", e.config.ContentHashFallback,
		"dry_run", e.config.DryRun,
	)

	count, err := e.store.CountMessagesWithoutRFC822ID(
		e.config.AccountSourceIDs...,
	)
	if err != nil {
		return nil, fmt.Errorf("count messages without rfc822 id: %w", err)
	}

	var backfilledCount int64
	if count > 0 && e.config.DryRun {
		e.logger.Info(
			"dry-run: backfill needed before dedup can run — "+
				"messages missing rfc822_message_id will be skipped",
			"count", count)
		backfilledCount = -count // negative signals "needed but skipped"
	} else if count > 0 {
		e.logger.Info("backfilling rfc822_message_id from stored MIME",
			"count", count)
		var backfillFailed int64
		backfilledCount, backfillFailed, err = e.store.BackfillRFC822IDs(
			e.config.AccountSourceIDs,
			func(done, total int64) {
				e.logger.Info("backfill progress",
					"done", done, "total", total)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("backfill rfc822 ids: %w", err)
		}
		if backfilledCount > 0 {
			e.logger.Info("backfilled rfc822_message_id",
				"count", backfilledCount)
		}
		if backfillFailed > 0 {
			e.logger.Warn("backfill: some messages could not be parsed",
				"failed", backfillFailed)
		}
	}

	totalMessages, err := e.store.CountActiveMessages(
		e.config.AccountSourceIDs...,
	)
	if err != nil {
		return nil, fmt.Errorf("count active messages: %w", err)
	}

	storeGroups, err := e.store.FindDuplicatesByRFC822ID(
		e.config.AccountSourceIDs...,
	)
	if err != nil {
		return nil, fmt.Errorf("find duplicates: %w", err)
	}

	report := &Report{
		TotalMessages:   totalMessages,
		BySourcePair:    make(map[string]int),
		BackfilledCount: backfilledCount,
	}

	for _, sg := range storeGroups {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msgs, err := e.store.GetDuplicateGroupMessages(
			sg.RFC822MessageID, e.config.AccountSourceIDs...,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"get group messages for %s: %w",
				sg.RFC822MessageID, err,
			)
		}
		if len(msgs) < 2 {
			continue
		}

		group := DuplicateGroup{
			Key:     sg.RFC822MessageID,
			KeyType: "message-id",
		}
		for _, m := range msgs {
			matched := false
			if m.FromEmail != "" {
				if addrs := e.config.IdentityAddressesBySource[m.SourceID]; addrs != nil {
					_, matched = addrs[store.NormalizeIdentifierForCompare(m.FromEmail)]
				}
			}
			group.Messages = append(group.Messages, DuplicateMessage{
				ID:               m.ID,
				SourceID:         m.SourceID,
				SourceType:       m.SourceType,
				SourceIdentifier: m.SourceIdentifier,
				SourceMessageID:  m.SourceMessageID,
				Subject:          m.Subject,
				SentAt:           m.SentAt,
				HasRawMIME:       m.HasRawMIME,
				LabelCount:       m.LabelCount,
				ArchivedAt:       m.ArchivedAt,
				IsFromMe:         m.IsFromMe,
				HasSentLabel:     m.HasSentLabel,
				FromEmail:        m.FromEmail,
				MatchedIdentity:  matched,
			})
		}

		e.selectSurvivor(&group)
		report.Groups = append(report.Groups, group)
		report.BySourcePair[sourcePairKey(group.Messages)]++
	}

	if e.config.ContentHashFallback {
		// Exclude only losers (messages already selected for pruning) from
		// the content-hash pass, not survivors. A message missing
		// Message-ID can legitimately match the content of a survivor that
		// anchored a Message-ID group; survivors stay eligible so the
		// second pass can link orphan rows back to that anchor.
		//
		// Survivors are tracked separately so we can guarantee a survivor
		// of a Message-ID group cannot be demoted to a loser by the
		// content-hash pass (which would silently prune it after labels
		// from the Message-ID group's losers were already merged in).
		excludeIDs := make(map[int64]bool, len(report.Groups)*2)
		messageIDSurvivors := make(map[int64]bool, len(report.Groups))
		for _, g := range report.Groups {
			for j, m := range g.Messages {
				if j == g.Survivor {
					messageIDSurvivors[m.ID] = true
					continue
				}
				excludeIDs[m.ID] = true
			}
		}

		contentHashGroups, skipped, err := e.scanNormalizedHashGroups(excludeIDs)
		if err != nil {
			return nil, fmt.Errorf(
				"scan normalized content hashes: %w", err,
			)
		}
		report.SkippedDecompressionErrors = skipped
		for _, g := range contentHashGroups {
			// Spec § Detection: "A content-hash group with two Message-ID
			// survivors keeps both as winners (one per Message-ID group)."
			// Count how many Message-ID-pass survivors landed in this group;
			// if more than one, neither should be demoted — skip entirely.
			//
			// Spec § Survivor selection: "When any message in a duplicate
			// group looks like a sent copy, only sent copies are eligible to
			// survive." A separate skip handles the case where an MID
			// survivor and a sent-copy orphan share a content hash — forcing
			// the (non-sent) MID survivor in over the sent orphan would
			// silently violate the eligibility filter. Sent-copy detection
			// excludes the MID survivor itself; if the MID survivor is
			// already a sent copy, the override only confirms selectSurvivor's
			// choice, which is harmless.
			midSurvivorCount := 0
			hasSentOrphan := false
			for _, m := range g.Messages {
				if messageIDSurvivors[m.ID] {
					midSurvivorCount++
					continue
				}
				if m.IsSentCopy() {
					hasSentOrphan = true
				}
			}
			if midSurvivorCount > 1 {
				continue
			}
			if midSurvivorCount >= 1 && hasSentOrphan {
				continue
			}

			// If this content-hash group contains exactly one Message-ID
			// survivor that did not win the content-hash survivor selection,
			// force that survivor to win. Demoting a survivor that has already
			// absorbed labels from its Message-ID losers would silently destroy
			// that union when MergeDuplicates soft-deletes the demoted survivor.
			for j, m := range g.Messages {
				if j == g.Survivor {
					continue
				}
				if messageIDSurvivors[m.ID] {
					g.Survivor = j
					break
				}
			}
			report.Groups = append(report.Groups, g)
			report.ContentHashGroups++
			report.BySourcePair[sourcePairKey(g.Messages)]++
		}
	}

	report.DuplicateGroups = len(report.Groups)
	for _, g := range report.Groups {
		report.DuplicateMessages += len(g.Messages) - 1
	}

	maxSamples := min(10, len(report.Groups))
	report.SampleGroups = append(
		[]DuplicateGroup(nil), report.Groups[:maxSamples]...,
	)

	e.logger.Info("dedup scan done",
		"groups", report.DuplicateGroups,
		"messages_to_prune", report.DuplicateMessages,
		"content_hash_groups", report.ContentHashGroups,
		"backfilled", report.BackfilledCount,
		"skipped_decompression_errors", report.SkippedDecompressionErrors,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return report, nil
}

// rawWorkItem carries one compressed raw-MIME blob to a worker.
type rawWorkItem struct {
	candidate store.ContentHashCandidate
	rawData   []byte
	compress  string
}

// hashResult carries the normalized hash plus message metadata.
type hashResult struct {
	hash    string
	msg     DuplicateMessage
	skipped bool
}

// scanNormalizedHashGroups hashes raw MIME after stripping transport-specific
// headers. It skips messages already matched by the primary Message-ID pass.
// Returns the duplicate groups plus a count of candidates skipped due to
// zlib decompression failure.
func (e *Engine) scanNormalizedHashGroups(
	excludeIDs map[int64]bool,
) ([]DuplicateGroup, int, error) {
	candidates, err := e.store.GetAllRawMIMECandidates(
		e.config.AccountSourceIDs...,
	)
	if err != nil {
		return nil, 0, err
	}

	candidateMap := make(map[int64]store.ContentHashCandidate, len(candidates))
	for _, c := range candidates {
		if !excludeIDs[c.ID] {
			candidateMap[c.ID] = c
		}
	}
	if len(candidateMap) == 0 {
		return nil, 0, nil
	}

	ids := make([]int64, 0, len(candidateMap))
	for id := range candidateMap {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	numWorkers := runtime.NumCPU()
	if numWorkers > 16 {
		numWorkers = 16
	}
	if numWorkers > len(ids) {
		numWorkers = len(ids)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	work := make(chan rawWorkItem, numWorkers*4)
	results := make(chan hashResult, numWorkers*4)
	const maxDecompressionWarns = 5
	var decompressionFailures atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				raw := item.rawData
				if item.compress == "zlib" {
					r, err := zlib.NewReader(bytes.NewReader(raw))
					if err != nil {
						if decompressionFailures.Add(1) <= maxDecompressionWarns {
							e.logger.Warn("content-hash: zlib open failed",
								"message_id", item.candidate.ID, "err", err)
						}
						results <- hashResult{skipped: true}
						continue
					}
					decompressed, err := io.ReadAll(r)
					_ = r.Close()
					if err != nil {
						if decompressionFailures.Add(1) <= maxDecompressionWarns {
							e.logger.Warn("content-hash: zlib read failed",
								"message_id", item.candidate.ID, "err", err)
						}
						results <- hashResult{skipped: true}
						continue
					}
					raw = decompressed
				}

				matched := false
				if item.candidate.FromEmail != "" {
					if addrs := e.config.IdentityAddressesBySource[item.candidate.SourceID]; addrs != nil {
						_, matched = addrs[store.NormalizeIdentifierForCompare(item.candidate.FromEmail)]
					}
				}

				results <- hashResult{
					hash: sha256Hex(normalizeRawMIME(raw)),
					msg: DuplicateMessage{
						ID:               item.candidate.ID,
						SourceID:         item.candidate.SourceID,
						SourceType:       item.candidate.SourceType,
						SourceIdentifier: item.candidate.SourceIdentifier,
						SourceMessageID:  item.candidate.SourceMessageID,
						Subject:          item.candidate.Subject,
						SentAt:           item.candidate.SentAt,
						HasRawMIME:       true,
						LabelCount:       item.candidate.LabelCount,
						ArchivedAt:       item.candidate.ArchivedAt,
						IsFromMe:         item.candidate.IsFromMe,
						HasSentLabel:     item.candidate.HasSentLabel,
						FromEmail:        item.candidate.FromEmail,
						MatchedIdentity:  matched,
					},
				}
			}
		}()
	}

	type hashEntry struct {
		msgs []DuplicateMessage
	}
	hashMap := make(map[string]*hashEntry)
	skipped := 0
	collectDone := make(chan struct{})
	go func() {
		for r := range results {
			if r.skipped {
				skipped++
				continue
			}
			if entry, ok := hashMap[r.hash]; ok {
				entry.msgs = append(entry.msgs, r.msg)
			} else {
				hashMap[r.hash] = &hashEntry{msgs: []DuplicateMessage{r.msg}}
			}
		}
		close(collectDone)
	}()

	readErr := e.store.StreamMessageRaw(
		ids,
		func(messageID int64, rawData []byte, compression string) {
			c, ok := candidateMap[messageID]
			if !ok {
				return
			}
			dataCopy := make([]byte, len(rawData))
			copy(dataCopy, rawData)
			work <- rawWorkItem{
				candidate: c,
				rawData:   dataCopy,
				compress:  compression,
			}
		},
	)
	close(work)
	wg.Wait()
	close(results)
	<-collectDone

	if readErr != nil {
		return nil, skipped, fmt.Errorf("stream message raw: %w", readErr)
	}

	var groups []DuplicateGroup
	for hash, entry := range hashMap {
		if len(entry.msgs) < 2 {
			continue
		}
		g := DuplicateGroup{
			Key:      hash,
			KeyType:  "normalized-hash",
			Messages: entry.msgs,
		}
		e.selectSurvivor(&g)
		groups = append(groups, g)
	}
	if skipped > maxDecompressionWarns {
		e.logger.Warn("content-hash: additional zlib failures suppressed",
			"suppressed", skipped-maxDecompressionWarns)
	}
	return groups, skipped, nil
}

// transportHeaders vary across otherwise-identical copies of the same email.
var transportHeaders = map[string]bool{
	"Received":                   true,
	"Delivered-To":               true,
	"Return-Path":                true,
	"X-Received":                 true,
	"X-Gmail-Labels":             true,
	"X-Gmail-Received":           true,
	"X-Google-Smtp-Source":       true,
	"X-Gm-Message-State":         true,
	"Authentication-Results":     true,
	"Dkim-Signature":             true,
	"Arc-Seal":                   true,
	"Arc-Message-Signature":      true,
	"Arc-Authentication-Results": true,
	"X-Google-Dkim-Signature":    true,
	"X-Forwarded-To":             true,
	"X-Forwarded-For":            true,
	"X-Original-To":              true,
	"X-Apple-Mail-Labels":        true,
}

// normalizeRawMIME strips transport/export-specific headers before hashing.
func normalizeRawMIME(raw []byte) []byte {
	crlfEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	lfEnd := bytes.Index(raw, []byte("\n\n"))
	headerEnd := -1
	switch {
	case crlfEnd >= 0 && lfEnd >= 0:
		headerEnd = min(crlfEnd, lfEnd)
	case crlfEnd >= 0:
		headerEnd = crlfEnd
	case lfEnd >= 0:
		headerEnd = lfEnd
	}
	if headerEnd == -1 {
		return raw
	}

	headerSection := raw[:headerEnd]
	// Find the start of the actual body after the blank line.
	var bodyStart int
	switch {
	case bytes.HasPrefix(raw[headerEnd:], []byte("\r\n\r\n")):
		bodyStart = headerEnd + 4
	case bytes.HasPrefix(raw[headerEnd:], []byte("\n\n")):
		bodyStart = headerEnd + 2
	default:
		return raw
	}
	body := raw[bodyStart:]

	// Copy headerSection before appending to avoid mutating the
	// underlying raw buffer (headerSection is a sub-slice of raw).
	hdrBuf := make([]byte, len(headerSection)+4)
	copy(hdrBuf, headerSection)
	copy(hdrBuf[len(headerSection):], "\r\n\r\n")
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(hdrBuf)))
	mimeHeader, err := reader.ReadMIMEHeader()
	if err != nil {
		return raw
	}

	var kept []string
	for key := range mimeHeader {
		if !transportHeaders[textproto.CanonicalMIMEHeaderKey(key)] {
			kept = append(kept, key)
		}
	}
	sort.Strings(kept)

	var buf bytes.Buffer
	for _, key := range kept {
		for _, val := range mimeHeader[key] {
			fmt.Fprintf(&buf, "%s: %s\n", key, val)
		}
	}
	buf.WriteString("\n") // canonical header/body separator
	buf.Write(body)
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// selectSurvivor picks the best message to keep in a duplicate group.
func (e *Engine) selectSurvivor(group *DuplicateGroup) {
	if len(group.Messages) <= 1 {
		group.Survivor = 0
		return
	}

	priorityMap := make(map[string]int)
	for i, st := range e.config.SourcePreference {
		priorityMap[st] = i
	}

	candidates := allIndexes(len(group.Messages))
	var sentIdxs []int
	for _, i := range candidates {
		if group.Messages[i].IsSentCopy() {
			sentIdxs = append(sentIdxs, i)
		}
	}
	if len(sentIdxs) > 0 {
		candidates = sentIdxs
	}

	best := candidates[0]
	for _, i := range candidates[1:] {
		if e.isBetter(
			group.Messages[i], group.Messages[best], priorityMap,
		) {
			best = i
		}
	}
	group.Survivor = best
}

func allIndexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// isBetter returns true if candidate is a better survivor than current.
func (e *Engine) isBetter(
	candidate, current DuplicateMessage, priorityMap map[string]int,
) bool {
	candPri := sourcePriority(candidate.SourceType, priorityMap)
	currPri := sourcePriority(current.SourceType, priorityMap)
	if candPri != currPri {
		return candPri < currPri
	}
	if candidate.HasRawMIME != current.HasRawMIME {
		return candidate.HasRawMIME
	}
	if candidate.LabelCount != current.LabelCount {
		return candidate.LabelCount > current.LabelCount
	}
	if !candidate.ArchivedAt.IsZero() && !current.ArchivedAt.IsZero() {
		return candidate.ArchivedAt.Before(current.ArchivedAt)
	}
	return candidate.ID < current.ID
}

func sourcePriority(sourceType string, priorityMap map[string]int) int {
	if p, ok := priorityMap[sourceType]; ok {
		return p
	}
	return len(priorityMap)
}

// Execute merges every duplicate group: unions labels onto the
// survivor, soft-deletes the pruned duplicates, and — when
// DeleteDupsFromSourceServer is enabled AND a pruned copy shares a
// source_id with its survivor — writes a deletion manifest.
func (e *Engine) Execute(
	ctx context.Context, report *Report, batchID string,
) (*ExecutionSummary, error) {
	summary := &ExecutionSummary{BatchID: batchID}

	started := time.Now()
	e.logger.Info("dedup execute start",
		"batch", batchID,
		"account", e.config.Account,
		"groups", report.DuplicateGroups,
		"messages_to_prune", report.DuplicateMessages,
		"stage_remote_deletion", e.config.DeleteDupsFromSourceServer,
	)

	remoteByKey := make(map[remoteKey][]string)

	for i, group := range report.Groups {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}

		survivor := group.Messages[group.Survivor]
		survivorID := survivor.ID
		var dupIDs []int64
		for j, m := range group.Messages {
			if j == group.Survivor {
				continue
			}
			dupIDs = append(dupIDs, m.ID)

			if !e.config.DeleteDupsFromSourceServer {
				continue
			}
			if !remoteSourceTypes[m.SourceType] {
				continue
			}
			if m.SourceID != survivor.SourceID {
				continue
			}
			acct := m.SourceIdentifier
			if acct == "" {
				acct = e.config.Account
			}
			key := remoteKey{Account: acct, SourceType: m.SourceType}
			remoteByKey[key] = append(
				remoteByKey[key], m.SourceMessageID,
			)
		}

		mergeResult, err := e.store.MergeDuplicates(
			survivorID, dupIDs, batchID,
		)
		if err != nil {
			return summary, fmt.Errorf(
				"merge group %d (%s): %w", i, group.Key, err,
			)
		}

		summary.GroupsMerged++
		summary.MessagesRemoved += len(dupIDs)
		summary.LabelsTransferred += mergeResult.LabelsTransferred
		summary.RawMIMEBackfilled += mergeResult.RawMIMEBackfilled
	}

	if e.config.DeleteDupsFromSourceServer && len(remoteByKey) > 0 {
		staged, err := e.stageDeletionManifests(batchID, remoteByKey)
		if err != nil {
			return summary, err
		}
		summary.StagedManifests = staged
	}

	e.logger.Info("dedup execute done",
		"batch", batchID,
		"groups_merged", summary.GroupsMerged,
		"messages_removed", summary.MessagesRemoved,
		"labels_transferred", summary.LabelsTransferred,
		"raw_mime_backfilled", summary.RawMIMEBackfilled,
		"staged_manifests", len(summary.StagedManifests),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return summary, nil
}

func (e *Engine) stageDeletionManifests(
	batchID string,
	byKey map[remoteKey][]string,
) ([]StagedManifest, error) {
	if e.config.DeletionsDir == "" {
		return nil, fmt.Errorf(
			"deletions dir not configured but " +
				"DeleteDupsFromSourceServer is true",
		)
	}

	mgr, err := deletion.NewManager(e.config.DeletionsDir)
	if err != nil {
		return nil, fmt.Errorf("open deletion manager: %w", err)
	}

	keys := make([]remoteKey, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Account != keys[j].Account {
			return keys[i].Account < keys[j].Account
		}
		return keys[i].SourceType < keys[j].SourceType
	})

	// Single-type accounts keep the original manifest ID (no source-type
	// suffix) so existing consumers — and test fixtures — don't see a
	// rename. Only accounts contributing duplicates from more than one
	// source type need disambiguation.
	typesPerAccount := make(map[string]int)
	for k := range byKey {
		typesPerAccount[k.Account]++
	}

	var staged []StagedManifest
	for _, k := range keys {
		ids := dedupStrings(byKey[k])
		if len(ids) == 0 {
			continue
		}

		description := fmt.Sprintf("Dedup pruned duplicates (%s)", batchID)
		manifest := deletion.NewManifest(description, ids)
		if typesPerAccount[k.Account] > 1 {
			manifest.ID = manifestIDFor(batchID, k.Account+"-"+k.SourceType)
		} else {
			manifest.ID = manifestIDFor(batchID, k.Account)
		}
		manifest.CreatedBy = "dedup"
		manifest.Filters.Account = k.Account

		path := filepath.Join(
			mgr.PendingDir(), manifest.ID+".json",
		)
		if err := manifest.Save(path); err != nil {
			return staged, fmt.Errorf(
				"save manifest for %s: %w", k.Account, err,
			)
		}
		staged = append(staged, StagedManifest{
			Account:      k.Account,
			SourceType:   k.SourceType,
			ManifestID:   manifest.ID,
			MessageCount: len(ids),
		})
	}
	return staged, nil
}

func manifestIDFor(batchID, account string) string {
	return fmt.Sprintf("%s-%s", batchID, SanitizeFilenameComponent(account))
}

// SanitizeFilenameComponent strips or replaces characters that are unsafe
// for use in filenames, ensuring the result contains only alphanumeric,
// hyphens, and underscores (with @ and . replaced by hyphens).
func SanitizeFilenameComponent(a string) string {
	var b strings.Builder
	for _, r := range a {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == '@' || r == '.':
			b.WriteRune('-')
		}
	}
	s := b.String()
	if s == "" {
		s = "account"
	}
	if len(s) > 40 {
		sum := sha256.Sum256([]byte(a))
		s = s[:31] + "-" + hex.EncodeToString(sum[:4])
	}
	return s
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Undo restores every message with the given batch ID and cancels any
// pending deletion manifests that dedup created for that batch.
//
// Manifest cancellation is best-effort: if cancelling one manifest
// fails, the remaining manifests are still attempted, and any errors
// are joined into a single returned error alongside the restored row
// count and the list of manifests already in progress.
func (e *Engine) Undo(batchID string) (int64, []string, error) {
	started := time.Now()
	e.logger.Info("dedup undo start", "batch", batchID)

	restored, err := e.store.UndoDedup(batchID)
	if err != nil {
		e.logger.Warn("dedup undo failed",
			"batch", batchID,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return 0, nil, err
	}

	if e.config.DeletionsDir == "" {
		e.logger.Info("dedup undo done",
			"batch", batchID,
			"restored", restored,
			"manifests_cancelled", 0,
			"manifests_still_running", 0,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		return restored, nil, nil
	}

	mgr, err := deletion.NewManager(e.config.DeletionsDir)
	if err != nil {
		return restored, nil, fmt.Errorf("open deletion manager: %w", err)
	}
	pending, err := mgr.ListPending()
	if err != nil {
		return restored, nil, fmt.Errorf("list pending: %w", err)
	}
	inProgress, err := mgr.ListInProgress()
	if err != nil {
		return restored, nil, fmt.Errorf("list in-progress: %w", err)
	}

	var stillExecuting []string
	var cancelErrs []error
	cancelled := 0
	prefix := batchID + "-"
	for _, m := range pending {
		if !strings.HasPrefix(m.ID, prefix) {
			continue
		}
		if err := mgr.CancelManifest(m.ID); err != nil {
			cancelErrs = append(cancelErrs, fmt.Errorf(
				"cancel manifest %s: %w", m.ID, err,
			))
			continue
		}
		cancelled++
	}
	for _, m := range inProgress {
		if !strings.HasPrefix(m.ID, prefix) {
			continue
		}
		stillExecuting = append(stillExecuting, m.ID)
	}
	e.logger.Info("dedup undo done",
		"batch", batchID,
		"restored", restored,
		"manifests_cancelled", cancelled,
		"manifests_still_running", len(stillExecuting),
		"cancel_errors", len(cancelErrs),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	if len(cancelErrs) > 0 {
		return restored, stillExecuting, errors.Join(cancelErrs...)
	}
	return restored, stillExecuting, nil
}

// FormatReport renders a human-readable report of the scan results.
func (e *Engine) FormatReport(r *Report) string {
	var sb strings.Builder
	sb.WriteString("\n=== Deduplication Report ===\n\n")

	if r.BackfilledCount < 0 {
		fmt.Fprintf(&sb,
			"Note: %d messages need RFC822 Message-ID backfill "+
				"from stored MIME (skipped in dry-run).\n"+
				"These messages will be backfilled and included "+
				"when you re-run without --dry-run.\n\n",
			-r.BackfilledCount)
	} else if r.BackfilledCount > 0 {
		fmt.Fprintf(&sb,
			"Backfilled %d messages with RFC822 Message-ID "+
				"from stored MIME.\n\n",
			r.BackfilledCount)
	}

	if r.DuplicateGroups == 0 {
		sb.WriteString("No duplicates found.\n")
		return sb.String()
	}

	fmt.Fprintf(&sb, "Duplicate groups found: %d\n", r.DuplicateGroups)
	fmt.Fprintf(&sb, "Messages to prune:      %d\n", r.DuplicateMessages)
	if r.ContentHashGroups > 0 {
		fmt.Fprintf(&sb, "Content-hash groups:   %d\n", r.ContentHashGroups)
	}
	if r.SkippedDecompressionErrors > 0 {
		fmt.Fprintf(&sb,
			"Skipped (decompression error): %d "+
				"(see log for per-message details)\n",
			r.SkippedDecompressionErrors)
	}

	if len(r.BySourcePair) > 0 {
		sb.WriteString("\nBreakdown by source pair:\n")
		pairs := make([]string, 0, len(r.BySourcePair))
		for k := range r.BySourcePair {
			pairs = append(pairs, k)
		}
		sort.Strings(pairs)
		for _, pair := range pairs {
			fmt.Fprintf(&sb, "  %-20s %d groups\n",
				pair, r.BySourcePair[pair])
		}
	}

	sentGroups := 0
	for _, g := range r.Groups {
		for _, m := range g.Messages {
			if m.IsSentCopy() {
				sentGroups++
				break
			}
		}
	}
	if sentGroups > 0 {
		fmt.Fprintf(&sb,
			"\nSent-copy groups detected: %d "+
				"(survivor forced to a sent copy)\n",
			sentGroups)
	}

	if len(r.SampleGroups) > 0 {
		sb.WriteString("\nSample duplicate groups:\n")
		for i, g := range r.SampleGroups {
			label := g.Key
			if g.KeyType != "" && g.KeyType != "message-id" {
				label = fmt.Sprintf("%s (%s)", g.Key, g.KeyType)
			}
			fmt.Fprintf(&sb, "\n  Group %d: %s\n", i+1, label)
			for j, m := range g.Messages {
				marker := "  "
				if j == g.Survivor {
					marker = "* "
				}
				sent := ""
				if m.IsSentCopy() {
					sent = " [sent]"
				}
				fmt.Fprintf(&sb,
					"    %s[%s:%s]%s %s "+
						"(labels: %d, raw: %v)\n",
					marker, m.SourceType, m.SourceIdentifier,
					sent, m.Subject, m.LabelCount, m.HasRawMIME,
				)
			}
		}
	}

	return sb.String()
}

// FormatMethodology returns a detailed explanation of how dedup works.
func (e *Engine) FormatMethodology() string {
	var sb strings.Builder
	sb.WriteString("\n=== Deduplication Methodology ===\n\n")

	sb.WriteString("Scope:\n")
	switch {
	case e.config.ScopeIsCollection && len(e.config.AccountSourceIDs) > 1:
		fmt.Fprintf(&sb,
			"  Scoped to collection: %s (%d account(s)). "+
				"Cross-account dedup\n"+
				"  is enabled within this collection.\n",
			e.config.Account, len(e.config.AccountSourceIDs))
	case e.config.ScopeIsCollection:
		// Single-member collection — wording matches the
		// account-scope branch since no cross-account merging can
		// happen with one source.
		fmt.Fprintf(&sb,
			"  Scoped to collection: %s (1 account). Source "+
				"boundaries are\n  never crossed (collection has "+
				"only one member).\n",
			e.config.Account)
	case e.config.Account != "":
		fmt.Fprintf(&sb,
			"  Scoped to account: %s. Source boundaries are "+
				"never crossed.\n",
			e.config.Account)
	case len(e.config.AccountSourceIDs) > 0:
		fmt.Fprintf(&sb,
			"  Scoped to %d source(s). Source boundaries are "+
				"never crossed.\n",
			len(e.config.AccountSourceIDs))
	default:
		sb.WriteString(
			"  No scope specified — only messages that appear " +
				"twice in the\n" +
				"  SAME account are eligible. To compare across " +
				"accounts, group\n" +
				"  them in a collection and rerun with " +
				"--collection <name>.\n",
		)
	}
	sb.WriteString("\n")

	sb.WriteString("Detection:\n")
	sb.WriteString("  Message-ID is primary; content-hash is a " +
		"supplementary fallback.\n")
	sb.WriteString("  Messages are grouped by the RFC822 Message-ID " +
		"header.\n")
	sb.WriteString("  Messages missing that header are backfilled " +
		"from stored MIME\n")
	sb.WriteString("  before the scan runs.")
	if e.config.ContentHashFallback {
		sb.WriteString(" Every remaining message with stored MIME is then compared via\n")
		sb.WriteString("  a normalized raw-MIME hash that strips transport " +
			"headers such as\n")
		sb.WriteString("  Received, Delivered-To, X-Gmail-Labels, and " +
			"DKIM/ARC traces.\n")
		sb.WriteString("  The hash is byte-sensitive below the header " +
			"boundary, so two\n")
		sb.WriteString("  messages whose bodies differ only in line-ending " +
			"style (CRLF vs LF)\n")
		sb.WriteString("  will not match via content-hash.\n\n")
	} else {
		sb.WriteString(" Messages still without an ID are ignored.\n\n")
	}

	sb.WriteString("Survivor selection:\n")
	for i, st := range e.config.SourcePreference {
		fmt.Fprintf(&sb, "  %d. %s\n", i+1, st)
	}
	sb.WriteString("  Tiebreakers: has raw MIME > more labels > " +
		"earlier archived_at > lower id.\n\n")

	sb.WriteString("Sent messages:\n")
	if e.config.ScopeIsCollection && len(e.config.AccountSourceIDs) > 1 {
		sb.WriteString(
			"  Collection mode INTENTIONALLY merges messages " +
				"across the accounts in this\n" +
				"  collection. A message alice sent to bob will " +
				"have one copy in alice's\n" +
				"  Sent folder and one in bob's Inbox; if both " +
				"accounts are members of\n" +
				"  this collection, the loser will be hidden " +
				"locally (reversible via\n" +
				"  --undo). Remote deletion remains " +
				"same-source-only and will not\n" +
				"  touch a different account's mailbox. Only " +
				"add accounts to a collection\n" +
				"  when you actually want their copies merged.\n\n",
		)
	} else {
		sb.WriteString(
			"  Dedup NEVER merges messages across different " +
				"accounts. A message that\n" +
				"  alice sent to bob is two distinct mailbox " +
				"copies — one in alice's\n" +
				"  Sent folder and one in bob's Inbox. Both are " +
				"preserved independently\n" +
				"  because deleting either would alter the other " +
				"user's archive.\n\n",
		)
	}

	sb.WriteString("Merge behaviour:\n")
	sb.WriteString("  - Labels from every copy are unioned onto " +
		"the survivor.\n")
	sb.WriteString("  - Raw MIME is backfilled onto the survivor " +
		"if it lacks it.\n")
	sb.WriteString("  - Only raw MIME is backfilled; parsed " +
		"message_bodies are not.\n")
	sb.WriteString("    If a survivor is missing text for display, run\n")
	sb.WriteString("    'msgvault repair-encoding' or " +
		"'msgvault build-cache --full-rebuild'.\n")
	sb.WriteString("  - Pruned duplicates are hidden in the msgvault " +
		"database (reversible via --undo).\n")
	sb.WriteString("  - Remote mailboxes (Gmail, IMAP) are NEVER " +
		"modified by default.\n")

	return sb.String()
}

func sourcePairKey(msgs []DuplicateMessage) string {
	types := make(map[string]bool)
	for _, m := range msgs {
		types[m.SourceType] = true
	}
	sorted := make([]string, 0, len(types))
	for t := range types {
		sorted = append(sorted, t)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, "+")
}
