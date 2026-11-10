package fbmessenger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const instagramInbox = "your_instagram_activity/messages/inbox"

type instagramShard struct {
	Participants []rawJSONParticipant `json:"participants"`
	Title        string               `json:"title"`
	ThreadPath   string               `json:"thread_path"`
	Messages     []jsontext.Value     `json:"messages"`
}

type instagramPending struct {
	id, filename string
	shard        *instagramShard
	raw          jsontext.Value
	record       rawJSONMessage
	message      Message
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
	files, bad, err := numberedJSONFiles(td.Path)
	if err != nil {
		return nil, nil, err
	}
	summary.FilesSkipped += int64(len(bad))
	if len(bad) > 0 {
		summary.Incomplete = true
	}
	threadDir := filepath.ToSlash(filepath.Join(instagramInbox, td.Name))
	key := ""
	var pending []*instagramPending
	for _, name := range files {
		filename := filepath.Join(td.Path, name)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(filename)
		shard := &instagramShard{}
		if err == nil {
			err = json.Unmarshal(data, shard)
		}
		if err != nil || shard.Messages == nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			summary.FilesSkipped++
			summary.Incomplete = true
			continue
		}
		shardKey := DecodeMojibake(shard.ThreadPath)
		if shardKey == "" {
			shardKey = strings.TrimPrefix(threadDir, path.Dir(instagramInbox)+"/")
		}
		if key != "" && key != shardKey {
			return nil, nil, errors.New("conflicting thread_path across shards")
		}
		key = shardKey
		if previous, ok := keys[key]; ok && previous != threadDir {
			return nil, nil, errors.New("conflicting thread_path across directories")
		}
		keys[key] = threadDir
		for _, raw := range shard.Messages {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			summary.MessagesProcessed++
			var record rawJSONMessage
			var stamp struct {
				Timestamp *int64 `json:"timestamp_ms"`
			}
			if json.Unmarshal(raw, &record) != nil || json.Unmarshal(raw, &stamp) != nil || stamp.Timestamp == nil {
				summary.MessagesSkipped++
				summary.RecordsUnsupported++
				summary.Incomplete = true
				continue
			}
			for _, items := range [][]rawJSONPhoto{record.Photos, record.Videos, record.Audio, record.Files, record.Gifs, record.Media} {
				for _, item := range items {
					if strings.TrimSpace(item.URI) == "" {
						summary.MediaRejected++
						summary.Incomplete = true
					}
				}
			}
			if record.Sticker != nil && strings.TrimSpace(record.Sticker.URI) == "" {
				summary.MediaRejected++
				summary.Incomplete = true
			}
			message := instagramJSONMessage(record)
			message.SentAt = time.UnixMilli(*stamp.Timestamp).UTC()

			if message.Body == "" && len(message.Attachments) == 0 {
				summary.MessagesSkipped++
				summary.RecordsUnsupported++
				summary.Incomplete = true
				continue
			}
			pending = append(pending, &instagramPending{filename: filename, shard: shard, raw: raw, record: record, message: message})
		}
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
	for prefix, group := range groups {
		old := archivedGroups[prefix]
		sort.Slice(old, func(i, j int) bool {
			a, _ := strconv.Atoi(strings.TrimPrefix(old[i], prefix))
			b, _ := strconv.Atoi(strings.TrimPrefix(old[j], prefix))
			return a < b
		})
		maxOrdinal := -1
		exact := map[instagramOccurrence][]string{}
		evidence := map[string]instagramOccurrence{}
		for _, id := range old {
			n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
			if err == nil && n > maxOrdinal {
				maxOrdinal = n
			}
			var wrapper struct {
				Record jsontext.Value `json:"record"`
			}
			var record rawJSONMessage
			if json.Unmarshal(archived[id].Raw, &wrapper) != nil || json.Unmarshal(wrapper.Record, &record) != nil {
				continue
			}
			key := instagramEvidence(record)
			evidence[id] = key
			exact[key] = append(exact[key], id)
		}
		used := map[string]bool{}
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
		for content, items := range remaining {
			if len(items) == 1 && len(unused[content]) == 1 {
				items[0].id = unused[content][0]
			}
		}
		for _, p := range group {
			if p.id == "" {
				maxOrdinal++
				p.id = prefix + strconv.Itoa(maxOrdinal)
			}
		}
	}

	parsed := &Thread{instagramKey: key, Format: formatJSON}
	for _, p := range pending {
		message := p.message
		message.sourceID, message.rawData = p.id, p.raw
		message.instagramShard, message.instagramFilename = p.shard, p.filename
		parsed.Messages = append(parsed.Messages, message)
	}
	return parsed, archived, nil
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
