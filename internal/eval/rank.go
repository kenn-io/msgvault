package eval

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
func DedupeKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue // hit carried no id for this doc-key; not scorable
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
