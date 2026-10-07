package whatsapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// appleChatMarkerVersion names the marker layout and the message derivation
// it vouches for. Bump it when either changes so the next import compares
// every chat again.
const appleChatMarkerVersion = 1

// appleChatMarkerState is the Apple import's source sync cursor: one change
// marker per chat, valid only after the sync run that wrote it and under the
// same import context.
type appleChatMarkerState struct {
	Version int              `json:"v"`
	SyncID  int64            `json:"sync_id"`
	Context string           `json:"context"`
	Chats   map[int64]string `json:"chats"`
}

// appleChatAggregate summarizes the Apple rows one chat's messages derive
// from. Core Data increments Z_OPT on every save of a row and never reuses a
// Z_PK, so an edit raises a sum, a deletion lowers a count, and an insert
// raises the highest row ID.
type appleChatAggregate struct {
	Messages int64
	MaxRowID int64
	// LatestDate catches a new message that reuses the Z_PK and Z_OPT of one
	// lost when WhatsApp was restored from an older backup.
	LatestDate float64
	OptSum     int64
	// OptMix weights each Z_OPT by its row's mix, so revisions that shift
	// between rows without changing their total still change the marker.
	OptMix int64
	// RowSum and RowMix fingerprint which rows belong to the chat. Count, highest
	// Z_PK and Z_OPT total alone cannot tell that rows moved between chats.
	// RowMix sums a hash of each Z_PK, so two row sets with equal sums still
	// differ.
	RowSum       int64
	RowMix       int64
	Members      int64
	MemberRowSum int64
	MemberOptSum int64
	// Ambiguous marks a chat with rows lacking Z_OPT; it is always read.
	Ambiguous bool
}

// fetchAppleChatAggregates returns per-chat aggregates of ZWAMESSAGE and the
// group members its rows join. It reports false when the database has no
// Z_OPT columns, so every chat is read.
func fetchAppleChatAggregates(
	ctx context.Context, db *sql.DB,
) (map[int64]appleChatAggregate, bool, error) {
	for _, table := range []string{"ZWAMESSAGE", "ZWAGROUPMEMBER"} {
		var columns int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'Z_OPT'`, table,
		).Scan(&columns); err != nil {
			return nil, false, fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if columns == 0 {
			return nil, false, nil
		}
	}

	// mix scrambles Z_PK with a multiply, an xor-shift (SQLite has no xor
	// operator) and a second multiply, so sums of it share no algebra with
	// sums of Z_PK.
	rows, err := db.QueryContext(ctx, `
		WITH scrambled AS (
			SELECT ZCHATSESSION, Z_PK, Z_OPT, ZGROUPMEMBER, ZMESSAGEDATE,
			       (Z_PK * 2654435761) % 4294967291 AS x
			FROM ZWAMESSAGE
			WHERE ZCHATSESSION IS NOT NULL
		), mixed AS (
			SELECT *, ((x | (x >> 16)) - (x & (x >> 16))) * 73244475 % 4294967291 AS mix
			FROM scrambled
		)
		SELECT m.ZCHATSESSION, COUNT(*), MAX(m.Z_PK),
		       COALESCE(MAX(CAST(m.ZMESSAGEDATE AS REAL)), 0), COUNT(m.Z_OPT),
		       COALESCE(SUM(m.Z_OPT), 0), COALESCE(SUM(m.mix * m.Z_OPT), 0),
		       SUM(m.Z_PK), SUM(m.mix), COUNT(gm.Z_PK),
		       COALESCE(SUM(gm.Z_PK), 0), COUNT(gm.Z_OPT),
		       COALESCE(SUM(gm.Z_OPT), 0)
		FROM mixed m
		LEFT JOIN ZWAGROUPMEMBER gm ON gm.Z_PK = m.ZGROUPMEMBER
		GROUP BY m.ZCHATSESSION
	`)
	if err != nil {
		return nil, false, fmt.Errorf("aggregate Apple messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	aggregates := make(map[int64]appleChatAggregate)
	for rows.Next() {
		var chatRowID, messagesWithOpt, membersWithOpt int64
		var aggregate appleChatAggregate
		if err := rows.Scan(
			&chatRowID, &aggregate.Messages, &aggregate.MaxRowID, &aggregate.LatestDate,
			&messagesWithOpt,
			&aggregate.OptSum, &aggregate.OptMix, &aggregate.RowSum, &aggregate.RowMix,
			&aggregate.Members, &aggregate.MemberRowSum,
			&membersWithOpt, &aggregate.MemberOptSum,
		); err != nil {
			return nil, false, fmt.Errorf("scan Apple message aggregate: %w", err)
		}
		aggregate.Ambiguous = messagesWithOpt != aggregate.Messages ||
			membersWithOpt != aggregate.Members
		aggregates[chatRowID] = aggregate
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate Apple message aggregates: %w", err)
	}
	return aggregates, true, nil
}

// fetchAppleStoreIdentity returns the Core Data store UUID from Z_METADATA,
// which stays with a ChatStorage.sqlite across saves and differs between
// stores. It returns "" when the database carries none.
func fetchAppleStoreIdentity(ctx context.Context, db *sql.DB) (string, error) {
	var columns int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('Z_METADATA') WHERE name = 'Z_UUID'`,
	).Scan(&columns); err != nil {
		return "", fmt.Errorf("inspect Z_METADATA columns: %w", err)
	}
	if columns == 0 {
		return "", nil
	}
	var identity sql.NullString
	err := db.QueryRowContext(ctx, `SELECT Z_UUID FROM Z_METADATA LIMIT 1`).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Apple store identity: %w", err)
	}
	return identity.String, nil
}

// appleChatMarker returns the change marker for one chat, or "" when the chat
// has no usable marker and must be read. It includes the chat's name because a
// direct chat's sender takes it when the message row carries none.
func appleChatMarker(
	aggregates map[int64]appleChatAggregate, chat appleChat, conversationID int64,
) string {
	if aggregates == nil {
		return ""
	}
	// A chat without messages has a zero aggregate.
	aggregate := aggregates[chat.RowID]
	if aggregate.Ambiguous {
		return ""
	}
	// Stored as a short digest: the cursor is read with every source and
	// shouldn't carry contact names.
	sum := sha256.Sum256(fmt.Appendf(nil, "%d %q %q %d %d %v %d %d %d %d %d %d %d",
		conversationID, chat.RawJID, chat.Name, aggregate.Messages, aggregate.MaxRowID,
		aggregate.LatestDate,
		aggregate.OptSum, aggregate.OptMix, aggregate.RowSum, aggregate.RowMix,
		aggregate.Members, aggregate.MemberRowSum, aggregate.MemberOptSum,
	))
	return hex.EncodeToString(sum[:8])
}

// appleImportContext fingerprints the inputs outside a chat's own rows that
// shape its derived messages and sender names, including which database they
// came from: two databases imported into one source can have equal aggregates.
// Markers recorded under another context are ignored.
func appleImportContext(
	storeIdentity string,
	selfParticipantID int64,
	lidMap, pushNames map[string]string,
	duplicateStanzas map[string]struct{},
) (string, error) {
	encoded, err := json.Marshal(struct {
		Store      string              `json:"store"`
		Self       int64               `json:"self"`
		LID        map[string]string   `json:"lid"`
		PushNames  map[string]string   `json:"push_names"`
		Duplicates map[string]struct{} `json:"duplicates"`
	}{
		storeIdentity, selfParticipantID,
		lidMap, pushNames, duplicateStanzas,
	}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode Apple import context: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// previousAppleChatMarkers returns the markers recorded under importContext by
// the most recent sync of this source other than currentSyncID, or none. The
// markers hold only while that sync is still the latest run in any status:
// another run since, such as an Android import or an import of a different
// database that failed or was cancelled partway, may have rewritten messages
// the markers vouch for.
func (imp *Importer) previousAppleChatMarkers(
	ctx context.Context, source *store.Source, currentSyncID int64, importContext string,
) (map[int64]string, error) {
	none := map[int64]string{}
	if !source.SyncCursor.Valid || source.SyncCursor.String == "" {
		return none, nil
	}
	var state appleChatMarkerState
	// A cursor that does not decode is not an Apple marker cursor.
	decoded := json.Unmarshal([]byte(source.SyncCursor.String), &state) == nil
	if !decoded || state.Version != appleChatMarkerVersion || state.Context != importContext {
		return none, nil
	}
	last, err := imp.store.GetLatestSyncContext(ctx, source.ID, currentSyncID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, fmt.Errorf("read latest sync: %w", err)
	}
	if last == nil || last.ID != state.SyncID || last.Status != store.SyncStatusCompleted {
		return none, nil
	}
	return state.Chats, nil
}

// saveAppleChatMarkers records markers for the running sync. They take effect
// only once that sync completes; see previousAppleChatMarkers.
func (imp *Importer) saveAppleChatMarkers(
	ctx context.Context,
	sourceID, syncID int64,
	importContext string,
	markers map[int64]string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(appleChatMarkerState{
		Version: appleChatMarkerVersion,
		SyncID:  syncID,
		Context: importContext,
		Chats:   markers,
	}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode Apple chat markers: %w", err)
	}
	if err := imp.store.UpdateSourceSyncState(sourceID, string(encoded)); err != nil {
		return fmt.Errorf("save Apple chat markers: %w", err)
	}
	return nil
}
