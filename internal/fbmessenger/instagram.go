package fbmessenger

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const instagramInbox = "your_instagram_activity/messages/inbox"

type instagramPending struct {
	id      string
	record  rawJSONMessage
	message Message
	skip    bool
}

type instagramContent struct {
	body, kind, media   string
	extra               string
	duration            int64
	hasDuration, missed bool
}
type instagramOccurrence struct {
	content   instagramContent
	reactions string
}

func instagramEvidence(record rawJSONMessage) instagramOccurrence {
	keys := make([]string, len(record.Reactions))
	for i, r := range record.Reactions {
		keys[i] = instagramReactionKey(r)
	}
	sort.Strings(keys)
	reactions, _ := json.Marshal(keys)
	record.Reactions = nil
	content := instagramContent{body: instagramJSONMessage(record).Body, kind: record.Type, media: instagramMediaEvidence(record), extra: instagramExtraEvidence(record), missed: record.Missed}
	if record.CallDuration != nil {
		content.duration, content.hasDuration = *record.CallDuration, true
	}
	return instagramOccurrence{content, string(reactions)}
}

// instagramMediaEvidence keeps every media kind's path for loose matching.
func instagramMediaEvidence(record rawJSONMessage) string {
	kinds := [][]rawJSONPhoto{record.Photos, record.Videos, record.Audio, record.Files, record.Media, record.Gifs}
	encodedKinds := make([][]string, len(kinds))
	for k, entries := range kinds {
		for _, entry := range entries {
			entry.URI = path.Clean(DecodeMojibake(entry.URI))
			entry.CreationTimestamp = 0
			entry.Extra = nil
			encoded, _ := json.Marshal(entry)
			encodedKinds[k] = append(encodedKinds[k], string(encoded))
		}
		sort.Strings(encodedKinds[k])
	}
	encoded, _ := json.Marshal(encodedKinds)
	return string(encoded)
}

// instagramExtraEvidence is the fields the importer doesn't parse, in the
// record, its media, its share and its sticker.
func instagramExtraEvidence(record rawJSONMessage) string {
	extras := make([]string, 4)
	extras[0] = instagramCanonical(record.Extra)
	if record.Share != nil {
		extras[1] = instagramCanonical(record.Share.Extra)
	}
	if record.Sticker != nil {
		extras[2] = instagramCanonical(record.Sticker.Extra)
	}
	var media []string
	for kind, entries := range [][]rawJSONPhoto{record.Photos, record.Videos, record.Audio, record.Files, record.Media, record.Gifs} {
		for _, entry := range entries {
			encoded, _ := json.Marshal([]any{kind, path.Clean(DecodeMojibake(entry.URI)), entry.CreationTimestamp, instagramCanonical(entry.Extra)})
			media = append(media, string(encoded))
		}
	}
	sort.Strings(media)
	encodedMedia, _ := json.Marshal(media)
	extras[3] = string(encodedMedia)
	encoded, _ := json.Marshal(extras)
	return string(encoded)
}

// instagramReactionKey identifies a reaction by its text, actor and unparsed fields.
func instagramReactionKey(r rawJSONReaction) string {
	encoded, _ := json.Marshal([]string{DecodeMojibake(r.Actor), DecodeMojibake(r.Reaction), instagramCanonical(r.Extra)}, jsontext.AllowInvalidUTF8(true))
	return string(encoded)
}

// instagramCanonical is a JSON value with sorted keys, so key order never
// separates two copies of one record.
func instagramCanonical(value jsontext.Value) string {
	canonical := append(jsontext.Value(nil), value...)
	if canonical.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)) != nil {
		return string(value)
	}
	return string(canonical)
}

func parseInstagramThread(ctx context.Context, st *store.Store, sourceID int64, root string, td ThreadDir, summary *ImportSummary, keys map[string]string, archivedGroups map[string][]string) (*Thread, map[string]store.StoredMessage, error) {
	parsed, err := parseJSONThread(ctx, root, td.Path, &instagramJSONOptions{td: td, summary: summary, keys: keys})
	if err != nil {
		return nil, nil, err
	}
	key := parsed.instagramKey
	var pending []*instagramPending
	for _, message := range parsed.Messages {
		pending = append(pending, &instagramPending{record: *message.jsonRecord, message: message})
	}
	groups := map[string][]*instagramPending{}
	for _, p := range pending {
		prefix := fmt.Sprintf("ig:v1:%s:%s:", instagramHash(key), instagramFingerprint(p.record))
		groups[prefix] = append(groups[prefix], p)
	}
	var ids []string
	seenIDs := map[string]bool{}
	for prefix := range groups {
		for _, id := range archivedGroups[prefix] {
			ids = append(ids, id)
			seenIDs[id] = true
		}
	}
	if len(archivedGroups) > 0 {
		times := make([]time.Time, 0, len(pending))
		seen := make(map[int64]bool)
		for _, p := range pending {
			if !seen[p.record.TimestampMs] {
				seen[p.record.TimestampMs] = true
				times = append(times, time.UnixMilli(p.record.TimestampMs).UTC())
			}
		}
		candidates, err := st.SourceConversationMessageIDsAtTimesContext(ctx, sourceID, key, times)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range candidates {
			if !seenIDs[id] {
				ids = append(ids, id)
				seenIDs[id] = true
			}
		}
	}
	archived, err := st.StoredMessagesContext(ctx, sourceID, "instagram_json", ids)
	if err != nil {
		return nil, nil, err
	}
	instagramMarkPossibleDuplicates(pending, archived)
	used := map[string]bool{}
	var repairs []string
	for prefix, group := range groups {
		matchInstagramGroup(group, archivedGroups[prefix], archived, used, summary, &repairs)
	}
	for prefix, group := range groups {
		numberInstagramGroup(prefix, group, archivedGroups[prefix])
	}

	parsed.Messages = nil
	for _, p := range pending {
		if p.skip {
			continue
		}
		message := p.message
		message.sourceID = p.id
		message.rawData, err = instagramPreserveCandidates(message.rawData, archived[p.id])
		if err != nil {
			return nil, nil, err
		}
		parsed.Messages = append(parsed.Messages, message)
	}
	for _, id := range repairs {
		record, ok := instagramArchivedRecord(archived[id])
		if !ok {
			continue
		}
		message := instagramJSONMessage(record)
		message.sourceID, message.rawData, message.jsonRecord = id, archived[id].Raw, &record
		message.SentAt = time.UnixMilli(record.TimestampMs).UTC()
		parsed.Messages = append(parsed.Messages, message)
	}
	return parsed, archived, nil
}

// A sender change and two distinct senders can produce the same evidence.
// Keep both records and report the uncertainty without assigning an identity.
func instagramMarkPossibleDuplicates(pending []*instagramPending, archived map[string]store.StoredMessage) {
	senders := map[string]map[string]bool{}
	for _, stored := range archived {
		record, ok := instagramArchivedRecord(stored)
		if !ok || record.SenderName == "" {
			continue
		}
		name := DecodeMojibake(record.SenderName)
		record.SenderName = ""
		key := instagramFingerprint(record)
		if senders[key] == nil {
			senders[key] = map[string]bool{}
		}
		senders[key][name] = true
	}
	for _, p := range pending {
		name := DecodeMojibake(p.record.SenderName)
		if name == "" {
			continue
		}
		record := p.record
		record.SenderName = ""
		for other := range senders[instagramFingerprint(record)] {
			if other != name {
				p.message.possibleDuplicate = true
				break
			}
		}
	}
}

// Retain the evidence carried by a surplus row after its identity is resolved.
func instagramPreserveCandidates(raw []byte, stored store.StoredMessage) ([]byte, error) {
	var prior, current instagramRaw
	if len(stored.Raw) == 0 {
		return raw, nil
	}
	if err := json.Unmarshal(stored.Raw, &prior); err != nil {
		return nil, fmt.Errorf("decode archived Instagram record: %w", err)
	}
	if len(prior.Candidates) == 0 {
		return raw, nil
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, fmt.Errorf("decode Instagram record: %w", err)
	}
	current.Candidates = prior.Candidates
	return json.Marshal(current, jsontext.PreserveRawStrings(true))
}

func instagramArchivedRecord(stored store.StoredMessage) (rawJSONMessage, bool) {
	var wrapper instagramRaw
	var record rawJSONMessage
	if json.Unmarshal(stored.Raw, &wrapper) != nil || json.Unmarshal(wrapper.Record, &record) != nil {
		return record, false
	}
	if wrapper.DerivedReactions != nil {
		record.Reactions = *wrapper.DerivedReactions
	}
	return record, true
}

// matchInstagramGroup assigns archived IDs sharing a fingerprint prefix: first
// by exact content and reactions, then by content when one copy remains.
func matchInstagramGroup(group []*instagramPending, old []string, archived map[string]store.StoredMessage, used map[string]bool, summary *ImportSummary, repairs *[]string) {
	ordinal := func(id string) int {
		n, _ := strconv.Atoi(id[strings.LastIndexByte(id, ':')+1:])
		return n
	}
	sort.Slice(old, func(i, j int) bool { return ordinal(old[i]) < ordinal(old[j]) })
	exact := map[instagramOccurrence][]string{}
	evidence := map[string]instagramOccurrence{}
	unreadable := false
	for _, id := range old {
		if record, ok := instagramArchivedRecord(archived[id]); ok {
			key := instagramEvidence(record)
			evidence[id] = key
			if !instagramIsSurplus(archived[id]) {
				exact[key] = append(exact[key], id)
			}
		} else {
			unreadable = true
		}
	}
	for _, p := range group {
		key := instagramEvidence(p.record)
		if candidates := exact[key]; len(candidates) > 0 {
			p.id = candidates[0]
			used[p.id] = true
			exact[key] = candidates[1:]
		}
	}
	remaining := map[instagramContent][]*instagramPending{}
	unused := map[instagramContent][]string{}
	uncertain := map[instagramContent]bool{}
	for _, p := range group {
		if p.id == "" {
			if unreadable {
				p.skip = true
				summary.Incomplete = true
				uncertain[instagramEvidence(p.record).content] = true
				continue
			}
			key := instagramEvidence(p.record).content
			remaining[key] = append(remaining[key], p)
		}
	}
	for _, id := range old {
		if key, ok := evidence[id]; ok && !used[id] {
			if uncertain[key.content] {
				*repairs = append(*repairs, id)
			} else {
				unused[key.content] = append(unused[key.content], id)
			}
		}
	}
	summary.Errors += len(uncertain)
	// Preserve content matches before considering surplus across changed fields.
	for content, items := range remaining {
		if len(items) == 1 && len(unused[content]) == 1 && (!instagramIsSurplus(archived[unused[content][0]]) || instagramCanResolveSurplus(archived[unused[content][0]], items[0].record)) {
			reconcileInstagramCopies(map[instagramContent][]*instagramPending{content: items}, map[instagramContent][]string{content: unused[content]}, used, summary, repairs, archived)
			if items[0].id != "" {
				delete(remaining, content)
				delete(unused, content)
			}
		}
	}
	reconcileInstagramExtraSurplus(remaining, unused, used, summary, repairs, archived)
	reconcileInstagramCopies(remaining, unused, used, summary, repairs, archived)
	matchInstagramExtraChanges(remaining, unused, used, archived, summary, repairs)
}

// Reconcile surplus copies across unknown-field changes before treating the
// archived surplus and the retained original as separate content groups.
func reconcileInstagramExtraSurplus(remaining map[instagramContent][]*instagramPending, unused map[instagramContent][]string, used map[string]bool, summary *ImportSummary, repairs *[]string, archived map[string]store.StoredMessage) {
	loose := func(c instagramContent) instagramContent { c.extra = ""; return c }
	items, rows := map[instagramContent][]*instagramPending{}, map[instagramContent][]string{}
	crosses, hasSurplus := map[instagramContent]bool{}, map[instagramContent]bool{}
	kinds := map[instagramContent]int{}
	for content, list := range remaining {
		key := loose(content)
		items[key] = append(items[key], list...)
		kinds[key]++
		crosses[key] = crosses[key] || len(unused[content]) == 0
	}
	for content, ids := range unused {
		key := loose(content)
		rows[key] = append(rows[key], ids...)
		crosses[key] = crosses[key] || len(remaining[content]) == 0
		for _, id := range ids {
			hasSurplus[key] = hasSurplus[key] || instagramIsSurplus(archived[id])
		}
	}
	handled := map[instagramContent]bool{}
	for key, list := range items {
		if !crosses[key] || len(rows[key]) == 0 || (!hasSurplus[key] && len(list) <= len(rows[key]) && kinds[key] == 1 && instagramSameEvidence(rows[key], archived)) {
			continue
		}
		reconcileInstagramCopies(map[instagramContent][]*instagramPending{key: list}, map[instagramContent][]string{key: rows[key]}, used, summary, repairs, archived)
		handled[key] = true
	}
	for content := range remaining {
		if handled[loose(content)] {
			delete(remaining, content)
		}
	}
	for content := range unused {
		if handled[loose(content)] {
			delete(unused, content)
		}
	}
}

// Unmatched copies reuse interchangeable rows when only unknown fields changed.
// Otherwise preserve the old rows and reconcile any provable surplus.
func matchInstagramExtraChanges(remaining map[instagramContent][]*instagramPending, unused map[instagramContent][]string, used map[string]bool, archived map[string]store.StoredMessage, summary *ImportSummary, repairs *[]string) {
	loose := func(c instagramContent) instagramContent { c.extra = ""; return c }
	items, rows, kinds := map[instagramContent][]*instagramPending{}, map[instagramContent][]string{}, map[instagramContent]int{}
	for content, list := range remaining {
		if len(unused[content]) == 0 {
			items[loose(content)] = append(items[loose(content)], list...)
			kinds[loose(content)]++
		}
	}
	for content, ids := range unused {
		if free := instagramFreeRows(ids, used, archived); len(remaining[content]) == 0 && len(free) > 0 {
			rows[loose(content)] = append(rows[loose(content)], free...)
			sort.Strings(rows[loose(content)])
		}
	}
	for key, list := range items {
		if kinds[key] == 1 && len(list) <= len(rows[key]) {
			reuseInstagramRows(list, rows[key], used, summary, repairs, archived)
		} else if len(rows[key]) > 0 {
			reconcileInstagramCopies(map[instagramContent][]*instagramPending{key: list}, map[instagramContent][]string{key: rows[key]}, used, summary, repairs, archived)
		}
	}
}

// reuseInstagramRows gives copies the rows in order, or settles them as
// ambiguous when the rows differ by reactions or unknown fields the copies
// can't tell apart.
func reuseInstagramRows(list []*instagramPending, rows []string, used map[string]bool, summary *ImportSummary, repairs *[]string, archived map[string]store.StoredMessage) {
	if !instagramSameEvidence(rows, archived) {
		key := instagramContent{}
		reconcileInstagramCopies(map[instagramContent][]*instagramPending{key: list}, map[instagramContent][]string{key: rows}, used, summary, repairs, archived)
		return
	}
	for i, p := range list {
		p.id = rows[i]
		used[p.id] = true
	}
}

// instagramSameEvidence is whether no row differs from the others by reactions
// or unknown fields.
func instagramSameEvidence(ids []string, archived map[string]store.StoredMessage) bool {
	for _, id := range ids {
		first, _ := instagramArchivedRecord(archived[ids[0]])
		record, _ := instagramArchivedRecord(archived[id])
		if instagramEvidence(record) != instagramEvidence(first) {
			return false
		}
	}
	return true
}

// instagramFreeRows is the ids no copy has claimed and no surplus copy made.
func instagramFreeRows(ids []string, used map[string]bool, archived map[string]store.StoredMessage) []string {
	var free []string
	for _, id := range ids {
		if !used[id] && !instagramIsSurplus(archived[id]) {
			free = append(free, id)
		}
	}
	return free
}

// A shared reaction set on a surplus row is not evidence of its identity.
func instagramIsSurplus(stored store.StoredMessage) bool {
	var wrapper instagramRaw
	return json.Unmarshal(stored.Raw, &wrapper) == nil && wrapper.DerivedReactions != nil
}

// A surplus without shared reactions can be resolved by a retained candidate.
func instagramCanResolveSurplus(stored store.StoredMessage, record rawJSONMessage) bool {
	var wrapper instagramRaw
	if json.Unmarshal(stored.Raw, &wrapper) != nil || wrapper.DerivedReactions == nil || len(*wrapper.DerivedReactions) != 0 {
		return false
	}
	for _, candidate := range wrapper.Candidates {
		prior, ok := instagramArchivedRecord(store.StoredMessage{Raw: candidate})
		if ok && instagramEvidence(prior) == instagramEvidence(record) {
			return true
		}
	}
	return false
}

// reconcileInstagramCopies settles changed copies that share content with
// unmatched archived rows. One copy for one row keeps the row. When the export
// can't prove any copy is new, the copies are skipped. Surplus copies are
// added with only the reactions every copy has. Either way the run is
// incomplete, since which copy is which can't be told.
func reconcileInstagramCopies(remaining map[instagramContent][]*instagramPending, unused map[instagramContent][]string, used map[string]bool, summary *ImportSummary, repairs *[]string, archived map[string]store.StoredMessage) {
	for content, items := range remaining {
		old := unused[content]
		switch {
		case len(old) == 0:
		case len(items) == 1 && len(old) == 1 && (!instagramIsSurplus(archived[old[0]]) || instagramCanResolveSurplus(archived[old[0]], items[0].record)):
			items[0].id = old[0]
			used[old[0]] = true
		default:
			summary.Incomplete = true
			summary.Errors++
			*repairs = append(*repairs, old...)
			surplus := len(items) - len(old)
			if surplus > 0 {
				addInstagramSurplus(items, surplus)
			}
			for _, p := range items[max(surplus, 0):] {
				p.skip = true
			}
		}
	}
}

// addInstagramSurplus turns the first n copies into identical new messages
// built from the copy whose record sorts lowest, keeping only the reactions all
// copies share, and skips the rest.
func addInstagramSurplus(items []*instagramPending, n int) {
	bare := func(p *instagramPending) string {
		record := p.record
		record.Reactions = nil
		encoded, _ := json.Marshal(record)
		return string(encoded)
	}
	sort.SliceStable(items, func(i, j int) bool { return bare(items[i]) < bare(items[j]) })
	shared := map[string]int{}
	for _, p := range items {
		seen := map[string]bool{}
		for _, r := range p.record.Reactions {
			if key := instagramReactionKey(r); !seen[key] {
				seen[key] = true
				shared[key]++
			}
		}
	}
	base := *items[0]
	base.id, base.record.Reactions = "", nil
	for _, r := range items[0].record.Reactions {
		if shared[instagramReactionKey(r)] == len(items) {
			base.record.Reactions = append(base.record.Reactions, r)
		}
	}
	raw := instagramSurplusRaw(items, n, base.record.Reactions)
	base.message = instagramJSONMessage(base.record)
	base.message.SentAt = items[0].message.SentAt
	for i, p := range items[:n] {
		*p = base
		p.message.rawData = raw[i]
	}
}

func instagramSurplusRaw(items []*instagramPending, n int, reactions []rawJSONReaction) [][]byte {
	if reactions == nil {
		reactions = []rawJSONReaction{}
	}
	raw := make([][]byte, n)
	for i := range n {
		var envelope instagramRaw
		_ = json.Unmarshal(items[i].message.rawData, &envelope)
		for j := i; j < len(items); j += n {
			envelope.Candidates = append(envelope.Candidates, items[j].message.rawData)
		}
		envelope.DerivedReactions = &reactions
		raw[i], _ = json.Marshal(envelope, jsontext.PreserveRawStrings(true))
	}
	return raw
}

// numberInstagramGroup gives each unmatched message the next free ordinal.
func numberInstagramGroup(prefix string, group []*instagramPending, old []string) {
	maxOrdinal := -1
	for _, id := range old {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, prefix)); err == nil && n > maxOrdinal {
			maxOrdinal = n
		}
	}
	for _, p := range group {
		if p.id == "" && !p.skip {
			maxOrdinal++
			p.id = prefix + strconv.Itoa(maxOrdinal)
		}
	}
}

func instagramHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

// Fingerprints exclude reactions, shard position, title and media availability.
// Exact decoded display names are evidence; never fold them into slugs.
func instagramFingerprint(record rawJSONMessage) string {
	message := instagramJSONMessage(record)
	media := make([]string, 0, len(message.Attachments))
	for _, att := range message.Attachments {
		media = append(media, att.Kind+":"+path.Clean(att.URI))
	}
	sort.Strings(media)
	var share []string
	if record.Share != nil {
		share = []string{DecodeMojibake(record.Share.Link), DecodeMojibake(record.Share.ShareText), DecodeMojibake(record.Share.OriginalContentOwner)}
	}
	// Concrete strings, integers and slices have no failing marshalers or cycles;
	// AllowInvalidUTF8 below makes every possible string encodable.
	encoded, _ := json.Marshal(struct {
		Sender       string
		Timestamp    int64
		Content      string
		Share, Media []string
	}{message.SenderName, record.TimestampMs, DecodeMojibake(record.Content), share, media},
		// These options are part of the durable ig:v1 byte encoding.
		json.FormatNilSliceAsNull(true), jsontext.EscapeForHTML(true),
		jsontext.EscapeForJS(true), jsontext.AllowInvalidUTF8(true))
	return instagramHash(string(encoded))
}

// instagramThreadNames lists the header participants plus any sender the
// header omits, so every sender is a recipient of the thread's other messages.
func instagramThreadNames(thread *Thread) []string {
	var names []string
	seen := map[string]bool{"": true}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, p := range thread.Participants {
		add(p.Name)
	}
	for _, m := range thread.Messages {
		add(m.SenderName)
	}
	return names
}

type instagramParticipant struct {
	id          int64
	name, email string
}

// instagramThreadParticipants keeps retained participants separate from export names.
func instagramThreadParticipants(ctx context.Context, st *store.Store, sourceID int64, account string, thread *Thread) (map[string]int64, map[string]string, []instagramParticipant, error) {
	rows, err := st.DB().QueryContext(ctx, st.Rebind(`SELECT p.id, COALESCE(p.display_name, ''), COALESCE(p.email_address, '') FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id JOIN participants p ON p.id = cp.participant_id
		WHERE c.source_id = ? AND c.source_conversation_id = ? ORDER BY p.id`), sourceID, thread.instagramKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load archived Instagram participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var archived []instagramParticipant
	for rows.Next() {
		var p instagramParticipant
		if err := rows.Scan(&p.id, &p.name, &p.email); err != nil {
			return nil, nil, nil, err
		}
		archived = append(archived, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	nameToID, nameToEmail := map[string]int64{}, map[string]string{}
	var addrs []mime.Address
	for _, name := range instagramThreadNames(thread) {
		addr := instagramAddress(account, name)
		addrs = append(addrs, addr)
	}
	ids, err := st.EnsureParticipantsBatch(ctx, addrs)
	if err != nil {
		return nil, nil, nil, err
	}
	used := map[int64]bool{}
	for _, addr := range addrs {
		nameToID[addr.Name], nameToEmail[addr.Name] = ids[addr.Email], addr.Email
		used[ids[addr.Email]] = true
	}
	var retained []instagramParticipant
	for _, p := range archived {
		if !used[p.id] {
			retained = append(retained, p)
		}
	}
	if len(used)+len(retained) > 2 {
		thread.ConvType = "group_chat"
	}
	return nameToID, nameToEmail, retained, nil
}

func instagramAddress(account, name string) mime.Address {
	return mime.Address{Name: name, Email: instagramHash(account+"\x00"+name) + "@instagram.messages", Domain: "instagram.messages"}
}

// Instagram media references use the same escaped UTF-8 encoding as names.
func instagramJSONMessage(record rawJSONMessage) Message {
	message := metaJSONMessage(record, 0, "")
	if record.Share != nil {
		message.Body = renderShareBody(rawJSONMessage{Share: &rawJSONShare{Link: DecodeMojibake(record.Share.Link), ShareText: record.Share.ShareText}}, DecodeMojibake(record.Content))
		if len(message.Reactions) > 0 {
			message.Body = appendReactionSummary(message.Body, message.Reactions)
		}
	}
	for i := range message.Attachments {
		message.Attachments[i].URI = DecodeMojibake(message.Attachments[i].URI)
	}
	return message
}
