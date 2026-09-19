package eval

import "fmt"

// DedupeKeys collapses a ranked list of document keys so each distinct key
// appears once, at its best (earliest) rank, preserving relative order.
//
// This matters when the unit being scored is coarser than the unit being
// retrieved. msgvault retrieves *messages*, but qrels may judge *threads*
// (--doc-key=conversation). A four-message thread that fills ranks 1-4 is one
// retrieved document, not four: scoring the raw list would count the same
// thread four times, inflating precision@k and — because the duplicates
// displace other threads out of the top-k window — distorting recall@k too.
//
// The bug is invisible on flat, one-message-per-document corpora such as the
// TREC Legal collection, which is exactly why msgvault needs a threaded
// fixture of its own to catch it.
//
// Ordering matters, and it is the caller's job to get it right: collapsing
// must happen BEFORE the ranked list is cut to the requested depth. Retrieving
// N messages and then collapsing them yields however many distinct threads
// happen to sit among those N — not N distinct threads — so "R@100" would
// silently measure a much shallower list than it claims. Callers therefore
// over-fetch raw hits, collapse, and only then TruncateKeys to the depth the
// user asked for.
func DedupeKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for i, k := range keys {
		if k == "" {
			// A hit with no id for this doc-key can never be judged
			// relevant — no qrels file names an empty docid — but it still
			// occupied a rank a real user would have seen. Dropping it
			// outright, rather than giving it a key, would let every
			// relevant document below it shift up to fill the hole,
			// inflating MRR/AP/nDCG by a rank position the run didn't earn.
			// A key unique to its position keeps the slot occupied (it can
			// never collide with a real key, and giving two such hits the
			// same placeholder would wrongly collapse them as one document)
			// while still resolving to non-relevant, exactly like any other
			// hit no qrels row names.
			out = append(out, fmt.Sprintf("\x00unscorable:%d", i))
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// TruncateKeys cuts an already-collapsed ranked list to at most n keys. It is
// the second half of the collapse-then-truncate rule described on DedupeKeys:
// applied to the deduped list it yields n distinct documents, applied to the
// raw list it would not. A non-positive n returns nothing, matching the
// "retrieve nothing" reading of a zero depth rather than silently meaning
// "everything".
func TruncateKeys(keys []string, n int) []string {
	if n <= 0 {
		return nil
	}
	if len(keys) <= n {
		return keys
	}
	return keys[:n]
}

const (
	// OverFetchFactor is how many raw hits are requested per requested
	// distinct key on the first attempt, and the factor by which the pool
	// grows on each retry.
	OverFetchFactor = 4
	// MaxOverFetchFactor bounds the pool: a query that cannot fill the
	// requested depth with distinct keys costs a bounded amount of work
	// instead of walking the whole corpus.
	MaxOverFetchFactor = 64
	// maxRawFetch is an absolute ceiling on one raw fetch, so an absurd
	// --limit cannot overflow the multiplication or ask an engine for a
	// nonsensical page.
	maxRawFetch = 1 << 20
)

// OverFetchPlan returns the successive raw-hit depths a caller should try in
// order to end up with `limit` DISTINCT doc keys.
//
// When the doc-key is 1:1 with retrieved hits there is nothing to collapse and
// the plan is just [limit] — no wasted work, and no latency inflation in a
// command that reports latency. When it is not (--doc-key=conversation, where
// one thread can occupy many consecutive ranks), the plan over-fetches so the
// collapse still has `limit` distinct keys left afterwards, growing while the
// engine has more to give:
//
//	limit*4, limit*16, limit*64
//
// The caller stops early as soon as the depth is filled or the engine returns
// fewer hits than asked for. A plan is never empty.
func OverFetchPlan(limit int, collapses bool) []int {
	if limit <= 0 {
		return []int{0}
	}
	if !collapses {
		return []int{limit}
	}
	var plan []int
	for factor := OverFetchFactor; factor <= MaxOverFetchFactor; factor *= OverFetchFactor {
		n := maxRawFetch
		if limit <= maxRawFetch/factor {
			n = limit * factor
		}
		if n < limit {
			n = limit
		}
		if len(plan) > 0 && n <= plan[len(plan)-1] {
			break // the pool ceiling stopped the depth growing
		}
		plan = append(plan, n)
	}
	if len(plan) == 0 {
		plan = []int{limit}
	}
	return plan
}
