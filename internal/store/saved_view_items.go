package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/entrykey"
	"go.kenn.io/msgvault/internal/identityindex"
)

const MaxSavedViewItemsPerMode = 500
const MaxSavedViewItemsBytes = 128 << 10

var ErrSavedViewItemsLimit = errors.New("saved view items limit exceeded")
var ErrSavedViewItemsInvalid = errors.New("items_json_invalid")

type SavedViewItemRef struct {
	MessageID      int64 `json:"message_id,omitempty"`
	ConversationID int64 `json:"conversation_id,omitempty"`
	SourceID       int64 `json:"source_id"`
}
type SavedViewItemEntry struct {
	SavedViewItemRef
	AddedBy string `json:"added_by"`
}
type SavedViewItems struct {
	Version int                  `json:"version"`
	Include []SavedViewItemEntry `json:"include"`
	Exclude []SavedViewItemEntry `json:"exclude"`
}
type SavedViewItemChange struct {
	IncludeAdd, IncludeRemove, ExcludeAdd, ExcludeRemove []SavedViewItemRef
	AddedBy                                              string
}

func ValidateSavedViewItemsJSON(raw []byte) (SavedViewItems, error) {
	items := SavedViewItems{Version: 1, Include: []SavedViewItemEntry{}, Exclude: []SavedViewItemEntry{}}
	if len(raw) == 0 {
		return items, nil
	}
	if len(raw) > MaxSavedViewItemsBytes {
		return items, ErrSavedViewItemsLimit
	}
	items.Version = 0
	if err := json.Unmarshal(raw, &items, json.RejectUnknownMembers(true)); err != nil {
		return items, fmt.Errorf("%w: %w", ErrSavedViewItemsInvalid, err)
	}
	if items.Version != 1 {
		return items, ErrSavedViewItemsInvalid
	}
	if len(items.Include) > MaxSavedViewItemsPerMode || len(items.Exclude) > MaxSavedViewItemsPerMode {
		return items, ErrSavedViewItemsLimit
	}
	seen := map[SavedViewItemRef]bool{}
	for _, entries := range [][]SavedViewItemEntry{items.Include, items.Exclude} {
		for _, e := range entries {
			if e.SourceID <= 0 || e.MessageID < 0 || e.ConversationID < 0 || (e.MessageID == 0) == (e.ConversationID == 0) || seen[e.SavedViewItemRef] {
				return items, ErrSavedViewItemsInvalid
			}
			seen[e.SavedViewItemRef] = true
		}
	}
	return items, nil
}

func encodeSavedViewItems(items SavedViewItems) (any, error) {
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	if _, err = ValidateSavedViewItemsJSON(raw); err != nil {
		return nil, err
	}
	if len(items.Include)+len(items.Exclude) == 0 {
		return nil, nil
	}
	return string(raw), nil
}
func applySavedViewItemChange(items SavedViewItems, change SavedViewItemChange) SavedViewItems {
	remove := func(entries []SavedViewItemEntry, refs []SavedViewItemRef) []SavedViewItemEntry {
		set := map[SavedViewItemRef]bool{}
		for _, ref := range refs {
			set[ref] = true
		}
		out := make([]SavedViewItemEntry, 0, len(entries))
		for _, e := range entries {
			if !set[e.SavedViewItemRef] {
				out = append(out, e)
			}
		}
		return out
	}
	items.Include = remove(items.Include, change.IncludeRemove)
	items.Exclude = remove(items.Exclude, change.ExcludeRemove)
	for _, ref := range change.IncludeAdd {
		items.Exclude = remove(items.Exclude, []SavedViewItemRef{ref})
		items.Include = remove(items.Include, []SavedViewItemRef{ref})
		items.Include = append(items.Include, SavedViewItemEntry{ref, change.AddedBy})
	}
	for _, ref := range change.ExcludeAdd {
		items.Include = remove(items.Include, []SavedViewItemRef{ref})
		items.Exclude = remove(items.Exclude, []SavedViewItemRef{ref})
		items.Exclude = append(items.Exclude, SavedViewItemEntry{ref, change.AddedBy})
	}
	return items
}
func (s *Store) GetSavedViewItems(ctx context.Context, viewID int64) (SavedViewItems, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT items_json FROM saved_views WHERE id=?`, viewID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedViewItems{}, ErrSavedViewNotFound
	}
	if err != nil {
		return SavedViewItems{}, err
	}
	items, err := ValidateSavedViewItemsJSON(raw)
	if err != nil {
		return items, err
	}
	live, _, err := s.liveSavedViewItems(ctx, items)
	return live, err
}
func (s *Store) ChangeSavedViewItems(ctx context.Context, viewID, expectedRevision int64, change SavedViewItemChange) (*SavedView, error) {
	return s.changeSavedViewItems(ctx, viewID, expectedRevision, change, nil)
}
func (s *Store) changeSavedViewItems(ctx context.Context, viewID, revision int64, change SavedViewItemChange, input *SavedViewInput) (*SavedView, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT revision,items_json FROM saved_views WHERE id=?`, viewID).Scan(&current, &raw); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSavedViewNotFound
	}
	if err != nil {
		return nil, err
	}
	if current != revision {
		return nil, ErrSavedViewRevisionConflict
	}
	items, err := ValidateSavedViewItemsJSON(raw)
	if err != nil {
		return nil, err
	}
	facts, err := s.liveItemFacts(ctx, tx.Tx, items)
	if err != nil {
		return nil, err
	}
	items, _ = liveItemsFromFacts(items, facts)
	changed := applySavedViewItemChange(items, change)
	applied := savedViewItemCounts(items, changed)
	value, err := encodeSavedViewItems(changed)
	if err != nil {
		return nil, err
	}
	sets := `items_json = ` + s.dialect.JSONBindExpr() + `, revision=revision+1,updated_at=` + s.dialect.Now()
	args := []any{value}
	if input != nil {
		sets += `, name=?, description=?, canonical_state=` + s.dialect.JSONBindExpr() + `,schema_version=?`
		args = append(args, input.Name, input.Description, string(input.CanonicalState), input.SchemaVersion)
	}
	args = append(args, viewID, revision)
	view, err := scanSavedView(tx.QueryRowContext(ctx, `UPDATE saved_views SET `+sets+` WHERE id=? AND revision=? RETURNING id,name,description,canonical_state,schema_version,revision,created_at,updated_at,items_json`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSavedViewRevisionConflict
	}
	if s.dialect.IsConflictError(err) {
		return nil, ErrSavedViewNameConflict
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	view.Applied = &applied
	return view, nil
}

// EntryKeyRejection describes a key that cannot identify a current entry.
type EntryKeyRejection struct {
	Key    string `json:"entry_key"`
	Reason string `json:"reason"`
}
type ResolvedEntry struct{ Ref SavedViewItemRef }

func (s *Store) ResolveEntryKeys(ctx context.Context, keys []string) (map[string]ResolvedEntry, []EntryKeyRejection, error) {
	resolved := map[string]ResolvedEntry{}
	rejected := []EntryKeyRejection{}
	// Bound each VALUES expression to 200 keys; preserve input order in errors.
	for start := 0; start < len(keys); start += 200 {
		batch := keys[start:min(start+200, len(keys))]
		type parsed struct {
			key string
			ref entrykey.Ref
		}
		valid := []parsed{}
		for _, key := range batch {
			ref, err := entrykey.Parse(key)
			if err != nil {
				rejected = append(rejected, EntryKeyRejection{key, "invalid_entry_key"})
				continue
			}
			valid = append(valid, parsed{key, ref})
		}
		if len(valid) == 0 {
			continue
		}
		values := []string{}
		args := []any{}
		for i, p := range valid {
			values = append(values, "(CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS TEXT))")
			args = append(args, i, p.ref.SourceID, p.ref.ConversationID, p.ref.SourceMessageID)
		}
		rows, err := s.db.QueryContext(ctx, `WITH wanted(ord,sid,cid,mid) AS (VALUES `+strings.Join(values, ",")+`) SELECT w.ord,m.id,m.source_id,COALESCE(m.message_type,''),COALESCE(c.conversation_type,'')
 FROM wanted w JOIN messages m ON m.source_id=w.sid AND w.cid=0 AND (m.source_message_id=w.mid OR ((m.source_message_id IS NULL OR m.source_message_id='') AND CAST(m.id AS TEXT)=w.mid)) LEFT JOIN conversations c ON c.id=m.conversation_id
 UNION ALL SELECT w.ord,0,c.source_id,'','' FROM wanted w JOIN conversations c ON c.id=w.cid AND c.source_id=w.sid WHERE w.cid>0`, args...)
		if err != nil {
			return nil, nil, err
		}
		found := map[int]bool{}
		bad := map[int]bool{}
		for rows.Next() {
			var ord int
			var id, sid int64
			var mt, ct string
			if err = rows.Scan(&ord, &id, &sid, &mt, &ct); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			found[ord] = true
			p := valid[ord]
			if id > 0 && identityindex.IsChat(mt, ct) {
				bad[ord] = true
				continue
			}
			resolved[p.key] = ResolvedEntry{SavedViewItemRef{MessageID: id, ConversationID: p.ref.ConversationID, SourceID: sid}}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, nil, err
		}
		for i, p := range valid {
			if bad[i] {
				delete(resolved, p.key)
				rejected = append(rejected, EntryKeyRejection{p.key, "use_conversation_key"})
			} else if !found[i] {
				rejected = append(rejected, EntryKeyRejection{p.key, "not_found"})
			}
		}
	}
	// Sort rejections by caller order, including malformed keys.
	order := map[string]int{}
	for i, k := range keys {
		order[k] = i
	}
	sort.SliceStable(rejected, func(i, j int) bool { return order[rejected[i].Key] < order[rejected[j].Key] })
	return resolved, rejected, nil
}

type itemSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// SavedViewItem is a live entry with its current Explore key.
type SavedViewItem struct {
	SavedViewItemEntry
	EntryKey string `json:"entry_key"`
	Kind     string `json:"kind"`
}
type SavedViewItemList struct {
	Include []SavedViewItem `json:"include"`
	Exclude []SavedViewItem `json:"exclude"`
}
type SavedViewItemsApplied struct {
	IncludeAdded   int `json:"include_added"`
	IncludeRemoved int `json:"include_removed"`
	ExcludeAdded   int `json:"exclude_added"`
	ExcludeRemoved int `json:"exclude_removed"`
}

func (s *Store) liveItemFacts(ctx context.Context, q itemSQL, items SavedViewItems) (map[SavedViewItemRef]SavedViewItem, error) {
	facts := map[SavedViewItemRef]SavedViewItem{}
	for _, conversation := range []bool{false, true} {
		ids := []int64{}
		for _, entries := range [][]SavedViewItemEntry{items.Include, items.Exclude} {
			for _, e := range entries {
				id := e.MessageID
				if conversation {
					id = e.ConversationID
				}
				if id > 0 {
					ids = append(ids, id)
				}
			}
		}
		for start := 0; start < len(ids); start += 200 {
			batch := ids[start:min(start+200, len(ids))]
			args := make([]any, len(batch))
			for i, id := range batch {
				args[i] = id
			}
			marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
			statement := `SELECT m.id,m.source_id,COALESCE(NULLIF(m.source_message_id,''),CAST(m.id AS TEXT)) FROM messages m WHERE m.id IN (` + marks + `)`
			if conversation {
				statement = `SELECT id,source_id,CAST(id AS TEXT) FROM conversations WHERE id IN (` + marks + `)`
			}
			rows, err := q.QueryContext(ctx, s.dialect.Rebind(statement), args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id, sid int64
				var provider string
				if err = rows.Scan(&id, &sid, &provider); err != nil {
					_ = rows.Close()
					return nil, err
				}
				ref := SavedViewItemRef{MessageID: id, SourceID: sid}
				kind := "message"
				if conversation {
					ref.MessageID = 0
					ref.ConversationID = id
					kind = "conversation"
				}
				facts[ref] = SavedViewItem{EntryKey: fmt.Sprintf("source:%d:%s:%s", sid, kind, provider), Kind: kind}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return facts, nil
}
func liveItemsFromFacts(items SavedViewItems, facts map[SavedViewItemRef]SavedViewItem) (SavedViewItems, int) {
	live := SavedViewItems{Version: 1, Include: []SavedViewItemEntry{}, Exclude: []SavedViewItemEntry{}}
	stale := 0
	for i, entries := range [][]SavedViewItemEntry{items.Include, items.Exclude} {
		for _, e := range entries {
			if _, ok := facts[e.SavedViewItemRef]; !ok {
				stale++
				continue
			}
			if i == 0 {
				live.Include = append(live.Include, e)
			} else {
				live.Exclude = append(live.Exclude, e)
			}
		}
	}
	return live, stale
}
func (s *Store) liveSavedViewItems(ctx context.Context, items SavedViewItems) (SavedViewItems, int, error) {
	facts, err := s.liveItemFacts(ctx, s.db.DB, items)
	if err != nil {
		return SavedViewItems{}, 0, err
	}
	live, stale := liveItemsFromFacts(items, facts)
	return live, stale, nil
}

// SavedViewItemsForView renders items from the same saved-row snapshot as revision.
func (s *Store) SavedViewItemsForView(ctx context.Context, view *SavedView) (SavedViewItemList, int, error) {
	items, err := ValidateSavedViewItemsJSON(view.ItemsJSON)
	if err != nil {
		return SavedViewItemList{}, 0, err
	}
	facts, err := s.liveItemFacts(ctx, s.db.DB, items)
	if err != nil {
		return SavedViewItemList{}, 0, err
	}
	live, stale := liveItemsFromFacts(items, facts)
	result := SavedViewItemList{Include: []SavedViewItem{}, Exclude: []SavedViewItem{}}
	for i, entries := range [][]SavedViewItemEntry{live.Include, live.Exclude} {
		for _, entry := range entries {
			item := facts[entry.SavedViewItemRef]
			item.SavedViewItemEntry = entry
			if i == 0 {
				result.Include = append(result.Include, item)
			} else {
				result.Exclude = append(result.Exclude, item)
			}
		}
	}
	return result, stale, nil
}

type ItemRemap struct{ Messages, Conversations, Sources map[int64]int64 }

func (s *Store) rewriteSavedViewItems(ctx context.Context, q itemSQL, rewrite func(SavedViewItems) SavedViewItems) error {
	rows, err := q.QueryContext(ctx, `SELECT id,items_json FROM saved_views WHERE items_json IS NOT NULL`)
	if err != nil {
		return err
	}
	type change struct {
		id    int64
		value any
	}
	changes := []change{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		items, err := ValidateSavedViewItemsJSON(raw)
		if err != nil {
			_ = rows.Close()
			return err
		}
		before, _ := json.Marshal(items)
		after := rewrite(items)
		encoded, err := encodeSavedViewItems(after)
		if err != nil {
			_ = rows.Close()
			return err
		}
		afterRaw, _ := json.Marshal(after)
		if string(before) != string(afterRaw) {
			changes = append(changes, change{id, encoded})
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, change := range changes {
		if _, err = q.ExecContext(ctx, s.dialect.Rebind(`UPDATE saved_views SET items_json=`+s.dialect.JSONBindExpr()+`,revision=revision+1,updated_at=`+s.dialect.Now()+` WHERE id=?`), change.value, change.id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) remapSavedViewItemsTx(ctx context.Context, tx *loggedTx, remap ItemRemap) error {
	// Dedup can cross sources, so derive the survivor source, too.
	survivorSources := map[int64]int64{}
	for _, id := range remap.Messages {
		var sid int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM messages WHERE id=?`, id).Scan(&sid); err != nil {
			return err
		}
		survivorSources[id] = sid
	}
	return s.rewriteSavedViewItems(ctx, tx.Tx, func(items SavedViewItems) SavedViewItems {
		out := SavedViewItems{Version: 1, Include: []SavedViewItemEntry{}, Exclude: []SavedViewItemEntry{}}
		seen := map[SavedViewItemRef]bool{}
		for mode, entries := range [][]SavedViewItemEntry{items.Exclude, items.Include} {
			for _, e := range entries {
				if id, ok := remap.Messages[e.MessageID]; ok {
					e.MessageID = id
					e.SourceID = survivorSources[id]
				}
				if id, ok := remap.Conversations[e.ConversationID]; ok {
					e.ConversationID = id
				}
				if id, ok := remap.Sources[e.SourceID]; ok {
					e.SourceID = id
				}
				if seen[e.SavedViewItemRef] {
					continue
				}
				seen[e.SavedViewItemRef] = true
				if mode == 0 {
					out.Exclude = append(out.Exclude, e)
				} else {
					out.Include = append(out.Include, e)
				}
			}
		}
		return out
	})
}
func (s *Store) pruneSavedViewItemsForSource(ctx context.Context, q itemSQL, sourceID int64) error {
	return s.rewriteSavedViewItems(ctx, q, func(items SavedViewItems) SavedViewItems {
		filter := func(entries []SavedViewItemEntry) []SavedViewItemEntry {
			out := make([]SavedViewItemEntry, 0, len(entries))
			for _, e := range entries {
				if e.SourceID != sourceID {
					out = append(out, e)
				}
			}
			return out
		}
		items.Include = filter(items.Include)
		items.Exclude = filter(items.Exclude)
		return items
	})
}

func savedViewItemCounts(before, after SavedViewItems) SavedViewItemsApplied {
	counts := SavedViewItemsApplied{}
	for mode, pair := range [][2][]SavedViewItemEntry{{before.Include, after.Include}, {before.Exclude, after.Exclude}} {
		old := map[SavedViewItemRef]bool{}
		next := map[SavedViewItemRef]bool{}
		for _, e := range pair[0] {
			old[e.SavedViewItemRef] = true
		}
		for _, e := range pair[1] {
			next[e.SavedViewItemRef] = true
		}
		added, removed := 0, 0
		for ref := range next {
			if !old[ref] {
				added++
			}
		}
		for ref := range old {
			if !next[ref] {
				removed++
			}
		}
		if mode == 0 {
			counts.IncludeAdded = added
			counts.IncludeRemoved = removed
		} else {
			counts.ExcludeAdded = added
			counts.ExcludeRemoved = removed
		}
	}
	return counts
}
