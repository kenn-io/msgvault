// Package rederive re-computes stored message columns from the verbatim
// provider payloads archived alongside them.
//
// Importers derive columns — body text, snippets, the search index, attachment
// classification — from a provider's payload at import time, then archive that
// payload as-is (message_raw.raw_format). Improving how a column is derived
// therefore leaves every already-archived message stale, and re-syncing repairs
// it only where the provider still holds the message and only at network speed.
//
// A registered pass reads the archive instead: offline, bounded by disk rather
// than an API, and able to repair messages the provider has since dropped.
// Registration lives here rather than in each importer so one command and one
// upgrade path serve all of them.
package rederive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Summary reports what a re-derivation pass changed.
type Summary struct {
	Duration                 time.Duration
	MessagesScanned          int64
	MessageMetadataRewritten int64
	BodiesRewritten          int64
	AttachmentsTagged        int64
	// Undecodable counts archived payloads that could not be parsed; they are
	// left untouched.
	Undecodable int64
	Errors      int64
	// unchanged is set only by passes that verify no derived writes committed.
	unchanged bool
}

// Add accumulates other into s, except Duration, which the caller owns.
func (s *Summary) Add(other *Summary) {
	if other == nil {
		return
	}
	s.MessagesScanned += other.MessagesScanned
	s.MessageMetadataRewritten += other.MessageMetadataRewritten
	s.BodiesRewritten += other.BodiesRewritten
	s.AttachmentsTagged += other.AttachmentsTagged
	s.Undecodable += other.Undecodable
	s.Errors += other.Errors
}

// Func re-derives every archived message of one source. progress may be nil.
type Func func(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*Summary, error)

type entry struct {
	fn      Func
	version string
}

var registry = map[string]entry{}

// Register associates a source type with its re-derivation pass.
//
// version identifies the derivation logic, not the schema: bump it whenever a
// change would produce different output for the same payload, so archives heal
// on their next sync. Registering a source type twice is a programming error
// and panics, since the second pass would silently shadow the first.
func Register(sourceType, version string, fn Func) {
	if _, dup := registry[sourceType]; dup {
		panic(fmt.Sprintf("rederive: source type %q registered twice", sourceType))
	}
	registry[sourceType] = entry{fn: fn, version: version}
}

// Lookup returns the pass registered for a source type.
func Lookup(sourceType string) (Func, string, bool) {
	e, ok := registry[sourceType]
	return e.fn, e.version, ok
}

func passes(sourceType, identifier string, retry bool) []entry {
	email := entry{fn: emailPass(LedgerKey(sourceType, identifier, emailVersion), retry), version: emailVersion}
	if fn, version, ok := Lookup(sourceType); ok {
		return []entry{{fn: fn, version: version}, email}
	}
	return []entry{email}
}

// SourceTypes lists every registered source type, sorted for stable output.
func SourceTypes() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LedgerKey names the applied_migrations entry recording that a source has
// been re-derived at a given version. Keying per source (rather than per source
// type) lets one account heal without blocking or repeating the others.
func LedgerKey(sourceType, identifier, version string) string {
	return fmt.Sprintf("rederive:%s:%s:%s", sourceType, identifier, version)
}

// Run executes a source's passes and records each in the ledger, whether or not
// the ledger already held it. This is the on-demand path; recording still matters
// here, or the next sync would repeat a full scan of an archive that is already
// current.
//
// The pass is recorded only on success, so a failed attempt is retried later
// rather than being silently marked done.
func Run(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (*Summary, error) {
	sum, _, err := run(ctx, s, sourceType, identifier, sourceID, progress, false)
	return sum, err
}

func runPass(ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string), pass entry) (*Summary, error) {
	sum, err := pass.fn(ctx, s, sourceID, progress)
	if err != nil {
		// A pass can fail after earlier message transactions committed. Make
		// those partial authoritative writes visible to cache maintenance even
		// though the repair remains retryable.
		if sum != nil && !sum.unchanged && (sum.MessagesScanned > 0 || sum.Errors > 0) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return sum, errors.Join(err, s.AdvanceDerivedDataRevisionContext(cleanupCtx))
		}
		return sum, err
	}
	if sum != nil && sum.Errors > 0 {
		if sum.unchanged {
			return sum, s.PublishPendingDerivedEmailRevisionContext(ctx)
		}
		return sum, s.AdvanceDerivedDataRevisionContext(ctx)
	}
	ledgerKey := LedgerKey(sourceType, identifier, pass.version)
	if sum == nil || sum.MessagesScanned == 0 || sum.unchanged {
		// Current rows need no invalidation of their own. Publish any pending
		// committed email writes before recording completion.
		return sum, s.MarkMigrationAppliedWithPendingDerivedRevisionContext(ctx, ledgerKey)
	}
	if err := s.MarkMigrationAppliedWithDerivedDataRevisionContext(ctx, ledgerKey); err != nil {
		return sum, err
	}
	return sum, nil
}

// RunIfStale runs each stale pass for a source, including retained MIME repair.
// ran reports whether any pass executed. This is the upgrade path, called from a sync.
func RunIfStale(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (sum *Summary, ran bool, err error) {
	return run(ctx, s, sourceType, identifier, sourceID, progress, true)
}

func run(ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string), onlyStale bool) (sum *Summary, ran bool, err error) {
	for _, pass := range passes(sourceType, identifier, onlyStale) {
		if onlyStale {
			applied, err := s.IsMigrationAppliedContext(ctx, LedgerKey(sourceType, identifier, pass.version), 1)
			if err != nil {
				return sum, ran, err
			}
			if applied {
				continue
			}
		}
		if sum == nil {
			sum = &Summary{}
		}
		partial, err := runPass(ctx, s, sourceType, identifier, sourceID, progress, pass)
		sum.Add(partial)
		if partial != nil {
			sum.Duration += partial.Duration
		}
		ran = true
		if err != nil {
			return sum, ran, err
		}
	}
	return sum, ran, nil
}
