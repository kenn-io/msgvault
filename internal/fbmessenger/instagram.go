package fbmessenger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type instagramOccurrence struct{ content, reactions string }

func instagramEvidence(record rawJSONMessage) instagramOccurrence {
	message := instagramJSONMessage(record)
	sort.Slice(message.Reactions, func(i, j int) bool {
		if message.Reactions[i].Actor != message.Reactions[j].Actor {
			return message.Reactions[i].Actor < message.Reactions[j].Actor
		}
		return message.Reactions[i].Reaction < message.Reactions[j].Reaction
	})
	reactions, _ := json.Marshal(message.Reactions)
	record.Reactions = nil
	return instagramOccurrence{instagramJSONMessage(record).Body, string(reactions)}
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
	for prefix := range groups {
		ids = append(ids, archivedGroups[prefix]...)
	}
	archived, err := st.StoredMessagesContext(ctx, sourceID, "instagram_json", ids)
	if err != nil {
		return nil, nil, err
	}
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
			instagramAddExact(exact, key, id, archived[id])
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
	remaining := map[string][]*instagramPending{}
	unused := map[string][]string{}
	uncertain := map[string]bool{}
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
	reconcileInstagramCopies(remaining, unused, used, summary, repairs, archived)
}

// A shared reaction set on a surplus row is not evidence of its identity.
func instagramIsSurplus(stored store.StoredMessage) bool {
	var wrapper instagramRaw
	return json.Unmarshal(stored.Raw, &wrapper) == nil && wrapper.DerivedReactions != nil
}

func instagramAddExact(exact map[instagramOccurrence][]string, key instagramOccurrence, id string, stored store.StoredMessage) {
	if !instagramIsSurplus(stored) {
		exact[key] = append(exact[key], id)
	}
}

// reconcileInstagramCopies settles changed copies that share content with
// unmatched archived rows. One copy for one row keeps the row. When the export
// can't prove any copy is new, the copies are skipped. Surplus copies are
// added with only the reactions every copy has. Either way the run is
// incomplete, since which copy is which can't be told.
func reconcileInstagramCopies(remaining map[string][]*instagramPending, unused map[string][]string, used map[string]bool, summary *ImportSummary, repairs *[]string, archived map[string]store.StoredMessage) {
	for content, items := range remaining {
		old := unused[content]
		switch {
		case len(old) == 0:
		case len(items) == 1 && len(old) == 1 && !instagramIsSurplus(archived[old[0]]):
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
	shared := map[rawJSONReaction]int{}
	for _, p := range items {
		seen := map[rawJSONReaction]bool{}
		for _, r := range p.record.Reactions {
			if !seen[r] {
				seen[r] = true
				shared[r]++
			}
		}
	}
	base := *items[0]
	base.id, base.record.Reactions = "", nil
	for _, r := range items[0].record.Reactions {
		if shared[r] == len(items) {
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
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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

func instagramAddress(account, name string) mime.Address {
	return mime.Address{Name: name, Email: instagramHash(account+"\x00"+name) + "@instagram.messages", Domain: "instagram.messages"}
}

// Instagram media references use the same escaped UTF-8 encoding as names.
func instagramJSONMessage(record rawJSONMessage) Message {
	message := metaJSONMessage(record, 0, "")
	if record.Share != nil {
		message.Body = renderShareBody(record, DecodeMojibake(record.Content))
		if len(message.Reactions) > 0 {
			message.Body = appendReactionSummary(message.Body, message.Reactions)
		}
	}
	for i := range message.Attachments {
		message.Attachments[i].URI = DecodeMojibake(message.Attachments[i].URI)
	}
	return message
}
