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

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const instagramInbox = "your_instagram_activity/messages/inbox"

type instagramPending struct {
	id, filename string
	shard        *rawJSONExport
	raw          jsontext.Value
	record       rawJSONMessage
	message      Message
	skip         bool
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

func parseInstagramThread(ctx context.Context, st *store.Store, sourceID int64, td ThreadDir, summary *ImportSummary, keys map[string]string, archivedGroups map[string][]string) (*Thread, map[string]store.StoredMessage, error) {
	parsed, err := parseJSONThread(ctx, "", td.Path, &instagramJSONOptions{td: td, summary: summary, keys: keys})
	if err != nil {
		return nil, nil, err
	}
	key := parsed.instagramKey
	var pending []*instagramPending
	for _, message := range parsed.Messages {
		var record rawJSONMessage
		if err := json.Unmarshal(message.rawData, &record); err != nil {
			return nil, nil, err
		}
		pending = append(pending, &instagramPending{filename: message.jsonFilename, shard: message.jsonShard, raw: message.rawData, record: record, message: message})
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
	for prefix, group := range groups {
		matchInstagramGroup(group, archivedGroups[prefix], archived, used, summary)
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
		message.sourceID, message.rawData = p.id, p.raw
		message.jsonShard, message.jsonFilename = p.shard, p.filename
		parsed.Messages = append(parsed.Messages, message)
	}
	return parsed, archived, nil
}

func instagramArchivedRecord(stored store.StoredMessage) (rawJSONMessage, bool) {
	var wrapper struct {
		Record jsontext.Value `json:"record"`
	}
	var record rawJSONMessage
	return record, json.Unmarshal(stored.Raw, &wrapper) == nil && json.Unmarshal(wrapper.Record, &record) == nil
}

// matchInstagramGroup assigns archived IDs sharing a fingerprint prefix: first
// by exact content and reactions, then by content when one copy remains.
func matchInstagramGroup(group []*instagramPending, old []string, archived map[string]store.StoredMessage, used map[string]bool, summary *ImportSummary) {
	ordinal := func(id string) int {
		n, _ := strconv.Atoi(id[strings.LastIndexByte(id, ':')+1:])
		return n
	}
	sort.Slice(old, func(i, j int) bool { return ordinal(old[i]) < ordinal(old[j]) })
	exact := map[instagramOccurrence][]string{}
	evidence := map[string]instagramOccurrence{}
	for _, id := range old {
		if record, ok := instagramArchivedRecord(archived[id]); ok {
			key := instagramEvidence(record)
			evidence[id] = key
			exact[key] = append(exact[key], id)
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
	for _, p := range group {
		if p.id == "" {
			key := instagramEvidence(p.record).content
			remaining[key] = append(remaining[key], p)
		}
	}
	for _, id := range old {
		if key, ok := evidence[id]; ok && !used[id] {
			unused[key.content] = append(unused[key.content], id)
		}
	}
	reconcileInstagramCopies(remaining, unused, used, summary)
}

// reconcileInstagramCopies settles changed copies that share content with
// unmatched archived rows. One copy for one row keeps the row. When the export
// can't prove any copy is new, the copies are skipped. Surplus copies are
// added with only the reactions every copy has. Either way the run is
// incomplete, since which copy is which can't be told.
func reconcileInstagramCopies(remaining map[string][]*instagramPending, unused map[string][]string, used map[string]bool, summary *ImportSummary) {
	for content, items := range remaining {
		old := unused[content]
		switch {
		case len(old) == 0:
		case len(items) == 1 && len(old) == 1:
			items[0].id = old[0]
			used[old[0]] = true
		default:
			summary.Incomplete = true
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
// copies share and rewriting raw to match, and skips the rest.
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
	var fields map[string]jsontext.Value
	_ = json.Unmarshal(base.raw, &fields)
	fields["reactions"], _ = json.Marshal(base.record.Reactions)
	base.raw, _ = json.Marshal(fields, json.Deterministic(true), jsontext.PreserveRawStrings(true))
	base.message = instagramJSONMessage(base.record)
	base.message.SentAt = items[0].message.SentAt
	for _, p := range items[:n] {
		*p = base
	}
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
