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
	Duration          time.Duration
	MessagesScanned   int64
	BodiesRewritten   int64
	AttachmentsTagged int64
	// Undecodable counts archived payloads that could not be parsed; they are
	// left untouched.
	Undecodable int64
	Errors      int64
}

// Add accumulates other into s, except Duration, which the caller owns.
func (s *Summary) Add(other *Summary) {
	if other == nil {
		return
	}
	s.MessagesScanned += other.MessagesScanned
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

// Run executes a source's pass and records it in the ledger, whether or not the
// ledger already held it. This is the on-demand path; recording still matters
// here, or the next sync would repeat a full scan of an archive that is already
// current.
//
// The pass is recorded only on success, so a failed attempt is retried later
// rather than being silently marked done.
func Run(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (*Summary, error) {
	fn, version, ok := Lookup(sourceType)
	if !ok {
		return nil, fmt.Errorf("no re-derivation pass registered for source type %q", sourceType)
	}
	sum, err := fn(ctx, s, sourceID, progress)
	if err != nil {
		// A pass can fail after earlier message transactions committed. Make
		// those partial authoritative writes visible to cache maintenance even
		// though the repair remains retryable.
		if sum != nil && (sum.MessagesScanned > 0 || sum.Errors > 0) {
			return sum, errors.Join(err, s.AdvanceDerivedDataRevision())
		}
		return sum, err
	}
	if sum != nil && sum.Errors > 0 {
		return sum, s.AdvanceDerivedDataRevision()
	}
	ledgerKey := LedgerKey(sourceType, identifier, version)
	if sum == nil || sum.MessagesScanned == 0 {
		// New and empty sources have no existing derived rows to invalidate.
		// Record the pass so sync does not repeat it, but keep a current cache
		// valid after the source's first import.
		return sum, s.MarkMigrationApplied(ledgerKey)
	}
	if err := s.MarkMigrationAppliedWithDerivedDataRevision(ledgerKey); err != nil {
		return sum, err
	}
	return sum, nil
}

// RunIfStale runs the registered pass for a source unless the ledger already
// records it at the current version. ran reports whether the pass actually
// executed: a source type with no registered pass, or one already recorded, is
// a no-op. This is the upgrade path, called from a sync.
func RunIfStale(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (sum *Summary, ran bool, err error) {
	_, version, ok := Lookup(sourceType)
	if !ok {
		return nil, false, nil
	}
	applied, err := s.IsMigrationApplied(LedgerKey(sourceType, identifier, version))
	if err != nil || applied {
		return nil, false, err
	}
	sum, err = Run(ctx, s, sourceType, identifier, sourceID, progress)
	return sum, true, err
}
