package imazingcsv

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/imazingidentity"
	"go.kenn.io/msgvault/internal/store"
)

const (
	// SourceType is the source namespace used for iMazing CSV archives.
	SourceType = "imazing_csv"
	// RawFormat labels the lossless named CSV row stored with each message.
	RawFormat = "imazing_csv_json"
)

// Options configures one iMazing CSV import.
type Options struct {
	// NotifyImports also requires the daemon global import opt-in.
	NotifyImports      bool
	Owner              string
	Timezone           string
	ContactsPath       string
	AttachmentsDir     string
	MaxAttachmentBytes int64
}

// Summary reports imported physical records and derived relationships.
type Summary struct {
	// ImportRunID identifies this non-resumable logical native import.
	ImportRunID        string `json:"import_run_id,omitempty"`
	Files              int
	Conversations      int
	Messages           int
	Participants       int
	AttachmentsStored  int
	AttachmentsMissing int
	AttachmentsSkipped int
	RepliesLinked      int
	RepliesUnresolved  int
	ContactsMatched    int
	ContactsTotal      int
}

// Importer imports one repeatable iMazing Messages CSV export.
type Importer struct {
	store *store.Store
	opts  Options
}

// NewImporter creates an importer using the caller-owned store.
func NewImporter(st *store.Store, opts Options) *Importer {
	return &Importer{store: st, opts: opts}
}

// validateTimezone rejects timezone names that cannot give offset-free CSV
// timestamps a stable meaning. Go resolves exactly the spelling "Local" to
// the host's time.Local (case variants do not fall back to it; they simply
// fail to load), so accepting it would decode the same export differently on
// every host while persisting the opaque name in the source sync config.
// LoadLocation returns time.Local itself for that spelling, so pointer
// identity rejects every host-dependent name without denying concrete IANA
// zones or the stable UTC alias.
func validateTimezone(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("iMazing CSV import timezone is required")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", name, err)
	}
	if loc == time.Local {
		return fmt.Errorf(
			"timezone %q is the host-dependent Local zone; pass a concrete IANA timezone name such as Europe/Berlin or UTC",
			name,
		)
	}
	return nil
}

type chatPlan struct {
	key            string
	title          string
	participants   map[string]participantIdentity
	participantIDs map[string]int64
	typeName       string
}

type plannedMessage struct {
	row                Row
	chat               *chatPlan
	sender             participantIdentity
	sourceMessageID    string
	occurrence         int
	occurrenceBase     string
	messageID          int64
	attachmentSnapshot *store.KeyedAttachmentSnapshot
	attachmentError    error
	replyParent        *plannedMessage
	replaceReply       bool
}

type sourceConfig struct {
	Timezone            string                    `json:"timezone"`
	AmbiguousTimePolicy string                    `json:"ambiguous_time_policy"`
	Other               map[string]jsontext.Value `json:",embed"`
}

// ImportPath imports an export root or CSV directory in one fenced sync run.
func (importer *Importer) ImportPath(ctx context.Context, path string) (summary Summary, retErr error) {
	if importer == nil || importer.store == nil {
		return Summary{}, errors.New("iMazing CSV importer requires a store")
	}
	// Offline imports cannot inherit a caller's live-capture authority.
	importer = &Importer{store: importer.store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill}), opts: importer.opts}
	owner, err := normalizeIdentity(importer.opts.Owner, "")
	if err != nil {
		return Summary{}, fmt.Errorf("normalize owner: %w", err)
	}
	if err := validateTimezone(importer.opts.Timezone); err != nil {
		return Summary{}, err
	}
	if err := importer.store.ValidateNativeImportChoice(ctx, SourceType, importer.opts.NotifyImports); err != nil {
		return Summary{}, err
	}
	layout, err := Discover(path)
	if err != nil {
		return Summary{}, err
	}

	source, err := importer.store.GetOrCreateSource(SourceType, owner.Value)
	if err != nil {
		return Summary{}, fmt.Errorf("get iMazing CSV source: %w", err)
	}
	syncID, err := importer.store.StartSyncContext(ctx, source.ID, "full")
	if err != nil {
		return Summary{}, fmt.Errorf("start iMazing CSV sync: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if failErr := importer.store.FailSync(syncID, retErr.Error()); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("fail iMazing CSV sync: %w", failErr))
		}
	}()

	syncStore := importer.store.ScopedToSync(source.ID, syncID)
	// iMazing is non-resumable: each native invocation gets a fresh logical
	// operation, even after a failed attempt. Silent runs still retain policy.
	logicalRun, err := syncStore.BeginNativeImportAttempt(ctx, source.ID, SourceType, importer.opts.NotifyImports, syncID, 0)
	if err != nil {
		return Summary{}, fmt.Errorf("begin iMazing CSV import: %w", err)
	}
	syncStore, err = syncStore.WithImportRun(ctx, logicalRun.ID)
	if err != nil {
		return Summary{}, fmt.Errorf("validate iMazing CSV import: %w", err)
	}
	if err := (&Importer{store: syncStore, opts: importer.opts}).checkAndStoreTimezone(source.ID); err != nil {
		return Summary{}, err
	}
	rows, err := ParseFiles(ctx, layout, importer.opts.Timezone)
	if err != nil {
		return Summary{}, err
	}
	plans, chats, err := planMessages(ctx, owner, source.Identifier, rows)
	if err != nil {
		return Summary{}, err
	}
	existingMessages, err := assignMessageIDs(ctx, syncStore, source.ID, plans)
	if err != nil {
		return Summary{}, err
	}
	retainedConversations := retainedArchivedConversations(plans, existingMessages)
	repliesLinked, repliesUnresolved := 0, 0
	if importer.opts.NotifyImports {
		repliesLinked, repliesUnresolved, err = prepareNotificationReplies(ctx, syncStore, source.ID, source.Identifier, importer.opts.Timezone, owner, plans, existingMessages, len(retainedConversations) > 0)
		if err != nil {
			return Summary{}, err
		}
	}

	attachmentsStored, attachmentsMissing, attachmentsSkipped := 0, 0, 0
	if importer.opts.NotifyImports {
		attachmentsStored, attachmentsMissing, attachmentsSkipped, err = prepareNotificationAttachments(ctx, layout, importer.opts, plans)
		if err != nil {
			return Summary{}, err
		}
	}
	messagesAdded, messagesUpdated, err := persistMessages(
		ctx, syncStore, source.ID, plans, existingMessages,
		retainedConversations, importer.opts.NotifyImports,
	)
	if err != nil {
		if statsErr := syncStore.RecomputeConversationStats(source.ID); statsErr != nil {
			return Summary{}, errors.Join(err,
				fmt.Errorf("recompute iMazing CSV conversation statistics after partial persistence: %w", statsErr))
		}
		return Summary{}, err
	}
	// Denormalized conversation statistics must reflect the committed
	// messages even when a later stage of the run fails, so recompute them
	// immediately after persistence and before reply, attachment, and
	// contact processing.
	if err := syncStore.RecomputeConversationStats(source.ID); err != nil {
		return Summary{}, fmt.Errorf("recompute iMazing CSV conversation statistics: %w", err)
	}
	if !importer.opts.NotifyImports {
		repliesLinked, repliesUnresolved, err = resolveReplies(
			ctx, syncStore, source.ID, source.Identifier, importer.opts.Timezone, owner,
			plans, len(retainedConversations) > 0,
		)
		if err != nil {
			return Summary{}, err
		}
	}
	if !importer.opts.NotifyImports {
		attachmentsStored, attachmentsMissing, attachmentsSkipped, err = importAttachments(ctx, syncStore, layout, importer.opts, plans)
		if err != nil {
			return Summary{}, err
		}
	}
	contactsMatched, contactsTotal := 0, 0
	if importer.opts.ContactsPath != "" {
		contactsMatched, contactsTotal, err = importContacts(importer.store, importer.opts.ContactsPath)
		if err != nil {
			return Summary{}, err
		}
	}
	// The run is not resumable, but the checkpoint counters feed the
	// source-status and sync-history views once the run completes.
	if err := syncStore.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: int64(len(plans)),
		MessagesAdded:     int64(messagesAdded),
		MessagesUpdated:   int64(messagesUpdated),
	}); err != nil {
		return Summary{}, fmt.Errorf("checkpoint iMazing CSV sync: %w", err)
	}
	if err := importer.store.CompleteSyncContext(ctx, syncID, ""); err != nil {
		return Summary{}, fmt.Errorf("complete iMazing CSV sync: %w", err)
	}

	return Summary{
		ImportRunID:        logicalRun.ID,
		Files:              len(layout.CSVFiles),
		Conversations:      len(chats),
		Messages:           len(plans),
		Participants:       countChatParticipants(chats),
		AttachmentsStored:  attachmentsStored,
		AttachmentsMissing: attachmentsMissing,
		AttachmentsSkipped: attachmentsSkipped,
		RepliesLinked:      repliesLinked,
		RepliesUnresolved:  repliesUnresolved,
		ContactsMatched:    contactsMatched,
		ContactsTotal:      contactsTotal,
	}, nil
}

func (importer *Importer) checkAndStoreTimezone(sourceID int64) error {
	source, err := importer.store.GetSourceByID(sourceID)
	if err != nil {
		return fmt.Errorf("reload iMazing CSV source: %w", err)
	}
	want := sourceConfig{Timezone: importer.opts.Timezone, AmbiguousTimePolicy: "earlier"}
	if source.SyncConfig.Valid && strings.TrimSpace(source.SyncConfig.String) != "" {
		var got sourceConfig
		if err := json.Unmarshal([]byte(source.SyncConfig.String), &got); err != nil {
			return fmt.Errorf("decode iMazing CSV source timezone: %w", err)
		}
		if got.Timezone != "" && got.Timezone != want.Timezone {
			return fmt.Errorf("iMazing CSV source timezone is %q, cannot import with %q", got.Timezone, want.Timezone)
		}
		if got.AmbiguousTimePolicy != "" && got.AmbiguousTimePolicy != want.AmbiguousTimePolicy {
			return fmt.Errorf("iMazing CSV ambiguous-time policy is %q, expected %q", got.AmbiguousTimePolicy, want.AmbiguousTimePolicy)
		}
		want.Other = got.Other
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("encode iMazing CSV source timezone: %w", err)
	}
	if err := importer.store.UpdateSourceSyncConfig(sourceID, string(encoded)); err != nil {
		return fmt.Errorf("store iMazing CSV source timezone: %w", err)
	}
	return nil
}

func planMessages(ctx context.Context, owner participantIdentity, sourceIdentifier string, rows []Row) ([]*plannedMessage, map[string]*chatPlan, error) {
	chats := make(map[string]*chatPlan)
	plans := make([]*plannedMessage, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		chatKey := conversationKey(row.ChatSession)
		chat := chats[chatKey]
		if chat == nil {
			chat = &chatPlan{
				key: chatKey, title: row.ChatSession,
				participants:   make(map[string]participantIdentity),
				participantIDs: make(map[string]int64),
			}
			chat.participants[owner.key()] = owner
			chats[chatKey] = chat
		}
		sender := owner
		if row.Direction == DirectionIncoming {
			var err error
			if row.SenderID != "" {
				sender, err = normalizeIdentity(row.SenderID, row.SenderName)
			} else if row.SenderName != "" {
				sender = syntheticIdentity(sourceIdentifier, chatKey, "sender", row.SenderName)
			} else {
				return nil, nil, fmt.Errorf("%s record %d: incoming sender ID or name is required", row.File, row.Record)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("%s record %d: normalize incoming sender: %w", row.File, row.Record, err)
			}
			addChatParticipant(chat, sender)
		}
		plans = append(plans, &plannedMessage{row: row, chat: chat, sender: sender})
	}

	chatKeys := make([]string, 0, len(chats))
	for key := range chats {
		chatKeys = append(chatKeys, key)
	}
	slices.Sort(chatKeys)
	for _, key := range chatKeys {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		chat := chats[key]
		addTitleParticipants(chat, sourceIdentifier)
		if len(chat.participants) > 2 {
			chat.typeName = "group_chat"
		} else {
			chat.typeName = "direct_chat"
		}
	}
	return plans, chats, nil
}
func addChatParticipant(chat *chatPlan, identity participantIdentity) {
	key := identity.key()
	existing, ok := chat.participants[key]
	if ok && existing.DisplayName != "" {
		return
	}
	chat.participants[key] = identity
}

func addTitleParticipants(chat *chatPlan, sourceIdentifier string) {
	// Titles can name a group or contain an ampersand in a single contact's
	// name. Only supplement a group already established by observed senders.
	if len(chat.participants) <= 2 {
		return
	}
	titleNames := make([]string, 0, 2)
	for titlePart := range strings.SplitSeq(chat.title, " & ") {
		name := strings.TrimSpace(titlePart)
		if name != "" {
			titleNames = append(titleNames, name)
		}
	}
	if len(titleNames) < 2 {
		return
	}
	for _, name := range titleNames {
		if chatHasDisplayName(chat, name) {
			continue
		}
		identity := syntheticIdentity(sourceIdentifier, chat.key, "member", name)
		addChatParticipant(chat, identity)
	}
}

func chatHasDisplayName(chat *chatPlan, name string) bool {
	want := normalizeChat(name)
	for _, identity := range chat.participants {
		if identity.DisplayName != "" && normalizeChat(identity.DisplayName) == want {
			return true
		}
	}
	return false
}

// assignMessageIDs gives every planned row a durable source message ID. Rows
// whose identity-bearing fields match share one base ID and are told apart by
// an occurrence suffix. Because a later export can insert, reorder, or drop
// physical rows, the suffix cannot follow the current row order alone: the
// assignment loads every archived message of the source in one query, discovers
// the archived occurrence prefix of each duplicate base in memory, and
// reconciles the current rows against that complete set, so an archived
// occurrence keeps its ID (and its labels, links, and metadata) whenever the
// current export still contains a row matching its recorded delivery and reply
// evidence. Only surplus rows receive newly allocated occurrence numbers, and
// an archived occurrence with no surviving row is left untouched instead of
// being remapped onto a different row.
func assignMessageIDs(
	ctx context.Context, st *store.Store, sourceID int64, plans []*plannedMessage,
) (map[string]store.MessageMetadataRecord, error) {
	type duplicateGroup struct {
		base  string
		plans []*plannedMessage
	}
	groupOrder := make([]string, 0, len(plans))
	groups := make(map[string]*duplicateGroup)
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		base := messageBaseID(plan.row, plan.chat.key, plan.sender)
		group := groups[base]
		if group == nil {
			group = &duplicateGroup{base: base}
			groups[base] = group
			groupOrder = append(groupOrder, base)
		}
		group.plans = append(group.plans, plan)
	}
	// One query covers every archived message of the source, including
	// occurrences beyond the current row count that earlier exports archived
	// for the same base. Probing for those IDs per duplicate group would cost
	// one extra query per message on archives where almost every message is a
	// group of one.
	existing, err := st.SourceMessageMetadata(sourceID)
	if err != nil {
		return nil, fmt.Errorf("load existing iMazing CSV messages: %w", err)
	}
	allocations, err := st.ImportOccurrenceAllocations(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("load durable iMazing occurrences: %w", err)
	}
	byBase := make(map[string][]store.ImportOccurrenceAllocation)
	for _, allocation := range allocations {
		byBase[allocation.Base] = append(byBase[allocation.Base], allocation)
	}
	for _, base := range groupOrder {
		reconcileDuplicateOccurrences(groups[base].base, groups[base].plans, existing, byBase[base]...)
	}
	return existing, nil
}

func duplicateOccurrenceID(base string, occurrence int) string {
	return imazingidentity.OccurrenceID(base, occurrence)
}

// rowEvidenceFingerprint carries the exported fields that messages within one
// duplicate group can differ on without changing their base ID. Delivery and
// reply evidence distinguish receipts and reply copies; subject, text, and
// attachment distinguish same-core rows that carry different content.
type rowEvidenceFingerprint struct {
	status     string
	delivered  string
	read       string
	replyingTo string
	subject    string
	text       string
	attachment string
}

func rowEvidenceFingerprintOf(row Row) rowEvidenceFingerprint {
	return rowEvidenceFingerprint{
		status: row.Status, delivered: row.DeliveredDate, read: row.ReadDate,
		replyingTo: row.ReplyingTo, subject: row.Subject, text: row.Text,
		attachment: row.Attachment,
	}
}

func storedEvidenceFingerprint(metadata sql.NullString) rowEvidenceFingerprint {
	var fingerprint rowEvidenceFingerprint
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" {
		return fingerprint
	}
	var decoded struct {
		Status     string `json:"status"`
		Delivered  string `json:"delivered_date"`
		Read       string `json:"read_date"`
		ReplyingTo string `json:"replying_to"`
		Subject    string `json:"subject"`
		Text       string `json:"text"`
		Attachment string `json:"attachment"`
	}
	if err := json.Unmarshal([]byte(metadata.String), &decoded); err != nil {
		return rowEvidenceFingerprint{}
	}
	return rowEvidenceFingerprint{
		status: decoded.Status, delivered: decoded.Delivered, read: decoded.Read,
		replyingTo: decoded.ReplyingTo, subject: decoded.Subject, text: decoded.Text,
		attachment: decoded.Attachment,
	}
}

// currentEvidenceMatchesArchived treats omitted current provider evidence as
// unknown rather than contradictory. iMazing can omit delivery/read fields on
// later exports; the remaining content evidence can still identify the same
// archived occurrence. A current non-empty value must always match exactly.
func currentEvidenceMatchesArchived(current, archived rowEvidenceFingerprint) bool {
	return (current.status == "" || current.status == archived.status) &&
		(current.delivered == "" || current.delivered == archived.delivered) &&
		(current.read == "" || current.read == archived.read) &&
		(current.replyingTo == "" || current.replyingTo == archived.replyingTo) &&
		(current.subject == "" || current.subject == archived.subject) &&
		(current.text == "" || current.text == archived.text) &&
		(current.attachment == "" || current.attachment == archived.attachment)
}

// archivedOccurrenceNumbers returns every occurrence number already archived
// for one duplicate base. Occurrence numbers are allocated as the smallest
// number that no loaded archived record and no earlier allocation in the same
// run claims, so the archived IDs of one base normally form a dense prefix
// starting at zero and probing until the first miss finds the complete set —
// including occurrences that a smaller later export no longer has rows for.
// A manually deleted archive message could leave a gap; probing then stops at
// the gap, and the remaining archived IDs above it still stay untouched
// because allocation skips every loaded archived ID.
func archivedOccurrenceNumbers(base string, existing map[string]store.MessageMetadataRecord) []int {
	numbers := make([]int, 0, 2)
	for number := 0; ; number++ {
		if _, archived := existing[duplicateOccurrenceID(base, number)]; !archived {
			return numbers
		}
		numbers = append(numbers, number)
	}
}

func reconcileDuplicateOccurrences(
	base string,
	plans []*plannedMessage,
	existing map[string]store.MessageMetadataRecord,
	allocations ...store.ImportOccurrenceAllocation,
) {
	occurrenceNumbers := archivedOccurrenceNumbers(base, existing)
	fingerprints := make([]rowEvidenceFingerprint, len(occurrenceNumbers))
	for index, number := range occurrenceNumbers {
		fingerprints[index] = storedEvidenceFingerprint(existing[duplicateOccurrenceID(base, number)].Metadata)
	}

	nativeIDByNumber := make(map[int]string, len(allocations))
	for _, allocation := range allocations {
		nativeIDByNumber[allocation.Number] = allocation.SourceMessageID
	}
	hashed := len(allocations) > 0
	if hashed {
		// A contracted first export may have adopted only some archive rows.
		// Retained live records and durable tombstones both participate in matching.
		byNumber := make(map[int]rowEvidenceFingerprint, len(occurrenceNumbers)+len(allocations))
		for i, number := range occurrenceNumbers {
			byNumber[number] = hashedOccurrenceFingerprint(fingerprints[i])
		}
		for _, allocation := range allocations {
			e := allocation.Evidence
			byNumber[allocation.Number] = rowEvidenceFingerprint{status: e[0], delivered: e[1], read: e[2], replyingTo: e[3], subject: e[4], text: e[5], attachment: e[6]}
		}
		occurrenceNumbers = make([]int, 0, len(byNumber))
		for number := range byNumber {
			occurrenceNumbers = append(occurrenceNumbers, number)
		}
		// Prefer an archived occurrence when identical evidence also matches a
		// tombstone. A contracted export must not recreate a deleted twin while
		// leaving its indistinguishable live sibling untouched.
		live := func(number int) bool {
			key := duplicateOccurrenceID(base, number)
			if canonical, ok := nativeIDByNumber[number]; ok {
				key = canonical
			}
			_, archived := existing[key]
			return archived
		}
		slices.SortFunc(occurrenceNumbers, func(a, b int) int {
			if live(a) != live(b) {
				if live(a) {
					return -1
				}
				return 1
			}
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
			return 0
		})
		fingerprints = make([]rowEvidenceFingerprint, len(occurrenceNumbers))
		for i, number := range occurrenceNumbers {
			fingerprints[i] = byNumber[number]
		}
	}
	reserved := make(map[int]bool, len(occurrenceNumbers))
	for _, number := range occurrenceNumbers {
		reserved[number] = true
	}
	rowAssigned := make([]bool, len(plans))
	occurrenceRow := make([]int, len(occurrenceNumbers))
	currentFingerprints := make([]rowEvidenceFingerprint, len(plans))
	for rowIndex, plan := range plans {
		currentFingerprints[rowIndex] = rowEvidenceFingerprintOf(plan.row)
		if hashed {
			currentFingerprints[rowIndex] = hashedOccurrenceFingerprint(currentFingerprints[rowIndex])
		}
	}
	for index := range occurrenceRow {
		occurrenceRow[index] = -1
	}
	// Phase 1 preserves exact evidence classes before considering wildcard
	// compatibility. This prevents a row with omitted evidence from taking an
	// archived occurrence for which an exact current row still exists.
	for index := range occurrenceNumbers {
		for rowIndex := range plans {
			if rowAssigned[rowIndex] || currentFingerprints[rowIndex] != fingerprints[index] {
				continue
			}
			occurrenceRow[index], rowAssigned[rowIndex] = rowIndex, true
			break
		}
	}
	// Phase 2 claims rows with one possible archived occurrence. Identical rows
	// share a candidate in CSV order; different evidence competing for the same
	// occurrence stays unresolved. Each claim can leave a wildcard row with one
	// match too.
	for {
		soleRows := make(map[int]int)
		for rowIndex := range plans {
			if rowAssigned[rowIndex] {
				continue
			}
			match, matches := -1, 0
			for index := range occurrenceNumbers {
				if occurrenceRow[index] >= 0 || !currentEvidenceMatchesArchived(
					currentFingerprints[rowIndex], fingerprints[index],
				) {
					continue
				}
				match, matches = index, matches+1
			}
			if matches == 1 {
				if previous, contested := soleRows[match]; contested {
					if previous >= 0 && currentFingerprints[rowIndex] != currentFingerprints[previous] {
						soleRows[match] = -1
					}
				} else {
					soleRows[match] = rowIndex
				}
			}
		}
		assigned := false
		for index := range occurrenceNumbers {
			if row, ok := soleRows[index]; ok && row >= 0 {
				occurrenceRow[index], rowAssigned[row] = row, true
				assigned = true
				break
			}
		}
		if !assigned {
			break
		}
	}
	// A one-row group has no competing occurrence whose identity could be
	// stolen, so mutable content edits retain the sole archived source ID.
	if len(plans) == 1 && len(occurrenceNumbers) == 1 && !rowAssigned[0] {
		occurrenceRow[0], rowAssigned[0] = 0, true
	}
	// Phase 3: surplus rows receive occurrence numbers that no archived
	// message and no earlier phase already claims. `existing` holds every
	// archived record of the source, so a newly allocated number can never
	// collide with an archived occurrence that this export has no row for.
	rowOccurrence := make([]int, len(plans))
	claimed := make(map[int]struct{}, len(plans))
	for index, row := range occurrenceRow {
		if row < 0 {
			continue
		}
		claimed[occurrenceNumbers[index]] = struct{}{}
		rowOccurrence[row] = occurrenceNumbers[index]
	}
	nextOccurrence := 0
	for rowIndex := range plans {
		if rowAssigned[rowIndex] {
			continue
		}
		for {
			if reserved[nextOccurrence] {
				nextOccurrence++
				continue
			}
			if _, taken := claimed[nextOccurrence]; taken {
				nextOccurrence++
				continue
			}
			if _, archived := existing[duplicateOccurrenceID(base, nextOccurrence)]; archived {
				nextOccurrence++
				continue
			}
			break
		}
		claimed[nextOccurrence] = struct{}{}
		rowOccurrence[rowIndex] = nextOccurrence
	}
	for rowIndex, plan := range plans {
		plan.sourceMessageID = duplicateOccurrenceID(base, rowOccurrence[rowIndex])
		if canonical, exists := nativeIDByNumber[rowOccurrence[rowIndex]]; exists {
			plan.sourceMessageID = canonical
		}
		plan.occurrence = rowOccurrence[rowIndex]
		plan.occurrenceBase = base
	}
}

type preparedMessagePersistence struct {
	plan           *plannedMessage
	participantIDs map[string]int64
	snapshot       store.ParticipantMessageSnapshot
}

func prepareMessagePersistence(sourceID int64, plan *plannedMessage, existing map[string]store.MessageMetadataRecord, retainedConversations map[string]struct{}) (*preparedMessagePersistence, error) {
	metadata, err := messageMetadata(plan.row, existing[plan.sourceMessageID].Metadata)
	if err != nil {
		return nil, fmt.Errorf("%s record %d: %w", plan.row.File, plan.row.Record, err)
	}
	raw, err := json.Marshal(plan.row.Raw)
	if err != nil {
		return nil, fmt.Errorf("%s record %d: marshal raw row: %w", plan.row.File, plan.row.Record, err)
	}
	fingerprint := hashedOccurrenceFingerprint(rowEvidenceFingerprintOf(plan.row))
	allocation := store.ImportOccurrenceAllocation{Base: plan.occurrenceBase, Number: plan.occurrence, SourceMessageID: plan.sourceMessageID, Evidence: [7]string{fingerprint.status, fingerprint.delivered, fingerprint.read, fingerprint.replyingTo, fingerprint.subject, fingerprint.text, fingerprint.attachment}}
	keys := make([]string, 0, len(plan.chat.participants))
	for key := range plan.chat.participants {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	participants := make([]store.ParticipantPersistData, len(keys))
	for i, key := range keys {
		participants[i] = participantPersistence(plan.chat.participants[key])
	}
	prepared := &preparedMessagePersistence{plan: plan}
	snapshot := store.ParticipantMessageSnapshot{SourceMessageID: plan.sourceMessageID, Participants: participants, Build: func(ids []int64) *store.MessagePersistData {
		chat := *plan.chat
		chat.participantIDs = make(map[string]int64, len(keys))
		for i, key := range keys {
			chat.participantIDs[key] = ids[i]
		}
		prepared.participantIDs = chat.participantIDs
		senderID := chat.participantIDs[plan.sender.key()]
		conversationParticipants := conversationParticipantRefs(&chat)
		_, preserveHistoricalConversation := retainedConversations[plan.chat.key]
		recipients, toValues := messageRecipients(&chat, plan.sender)
		delivery := store.MessageDeliveryEvidence{}
		if plan.row.DeliveredAt != nil {
			delivery.DeliveredAt = sql.NullTime{Time: *plan.row.DeliveredAt, Valid: true}
		}
		if plan.row.DeliveredAt != nil || plan.row.ReadAt != nil ||
			strings.EqualFold(plan.row.Status, "delivered") || strings.EqualFold(plan.row.Status, "read") {
			delivery.IsDelivered = sql.NullBool{Bool: true, Valid: true}
		}
		return &store.MessagePersistData{
			ImportOccurrence:        &allocation,
			ReplaceReply:            plan.replaceReply,
			ReplyToSourceMessageID:  notificationParentKey(plan),
			KeyedAttachmentSnapshot: plan.attachmentSnapshot,
			Message: &store.Message{
				SourceID: sourceID, SourceMessageID: plan.sourceMessageID,
				MessageType:    plan.row.Service,
				SentAt:         sql.NullTime{Time: plan.row.SentAt, Valid: true},
				SenderID:       sql.NullInt64{Int64: senderID, Valid: true},
				IsFromMe:       plan.row.Direction == DirectionOutgoing,
				Subject:        sql.NullString{String: plan.row.Subject, Valid: plan.row.Subject != ""},
				Snippet:        sql.NullString{String: plan.row.Text, Valid: plan.row.Text != ""},
				SizeEstimate:   int64(len(plan.row.Subject) + len(plan.row.Text)),
				HasAttachments: plan.row.Attachment != "", AttachmentCount: boolInt(plan.row.Attachment != ""),
			},
			Conversation: &store.ConversationPersistData{
				SourceConversationID: plan.chat.key,
				ConversationType:     plan.chat.typeName,
				Title:                plan.chat.title,
				Participants:         conversationParticipants,
				PreserveExistingType: preserveHistoricalConversation &&
					plan.chat.typeName == "direct_chat",
				PreserveExistingParticipants: preserveHistoricalConversation,
			},
			Delivery:   &delivery,
			Metadata:   &sql.NullString{String: metadata, Valid: true},
			BodyText:   sql.NullString{String: plan.row.Text, Valid: plan.row.Text != ""},
			RawMIME:    raw,
			RawFormat:  RawFormat,
			Recipients: recipients,
			// The CSV rows carry no label IDs; user-applied labels must
			// survive reruns instead of being replaced with an empty set.
			PreserveLabels: true,
			FTSBestEffort:  true,
			FTS: &store.FTSDoc{
				Subject: plan.row.Subject, Body: plan.row.Text,
				FromAddr: plan.sender.Value, ToAddrs: strings.Join(toValues, " "),
			},
		}
	}}
	prepared.snapshot = snapshot
	return prepared, nil
}

func notificationParentKey(plan *plannedMessage) string {
	if plan.replyParent != nil {
		return plan.replyParent.sourceMessageID
	}
	return ""
}

func persistMessages(ctx context.Context, st *store.Store, sourceID int64, plans []*plannedMessage, existing map[string]store.MessageMetadataRecord, retainedConversations map[string]struct{}, notify bool) (added, updated int, err error) {
	groups := make([][]*plannedMessage, 0, len(plans))
	if notify {
		groups = notificationMessageGroups(plans)
	} else {
		for _, plan := range plans {
			groups = append(groups, []*plannedMessage{plan})
		}
	}
	var rowErrors []error
	failed := make(map[string]bool)
	failGroup := func(group []*plannedMessage, err error) {
		rowErrors = append(rowErrors, err)
		for _, plan := range group {
			failed[plan.sourceMessageID] = true
		}
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return added, updated, errors.Join(append(rowErrors, err)...)
		}
		if notify {
			var unavailable []error
			for _, plan := range group {
				if plan.attachmentError != nil {
					unavailable = append(unavailable, plan.attachmentError)
				}
				if parent := plan.replyParent; parent != nil && failed[parent.sourceMessageID] {
					unavailable = append(unavailable, fmt.Errorf("%s record %d: required reply parent did not complete", plan.row.File, plan.row.Record))
				}
			}
			if len(unavailable) > 0 {
				failGroup(group, errors.Join(unavailable...))
				continue
			}
		}
		prepared := make([]*preparedMessagePersistence, 0, len(group))
		snapshots := make([]store.ParticipantMessageSnapshot, 0, len(group))
		for _, plan := range group {
			item, err := prepareMessagePersistence(sourceID, plan, existing, retainedConversations)
			if err != nil {
				return added, updated, err
			}
			prepared = append(prepared, item)
			snapshots = append(snapshots, item.snapshot)
		}
		var ids []int64
		if len(group) > 1 {
			ids, err = st.PersistImportMessageSnapshotsContext(ctx, sourceID, snapshots)
		} else {
			var id int64
			id, err = st.PersistMessageWithParticipantsContext(ctx, snapshots[0].Participants, snapshots[0].Build)
			ids = []int64{id}
		}
		if err != nil {
			rowErr := fmt.Errorf("%s record %d: persist message: %w", group[0].row.File, group[0].row.Record, err)
			if notify && errors.Is(err, store.ErrReadyAttachmentSnapshot) {
				failGroup(group, rowErr)
				continue
			}
			return added, updated, errors.Join(append(rowErrors, rowErr)...)
		}
		for i, item := range prepared {
			item.plan.chat.participantIDs = item.participantIDs
			item.plan.messageID = ids[i]
			if _, ok := existing[item.plan.sourceMessageID]; ok {
				updated++
			} else {
				added++
			}
		}
	}
	return added, updated, errors.Join(rowErrors...)
}

func retainedArchivedConversations(
	plans []*plannedMessage, existing map[string]store.MessageMetadataRecord,
) map[string]struct{} {
	current := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		current[plan.sourceMessageID] = struct{}{}
	}
	retained := make(map[string]struct{})
	for sourceMessageID, record := range existing {
		if _, present := current[sourceMessageID]; !present {
			if record.SourceConversationID.Valid {
				retained[record.SourceConversationID.String] = struct{}{}
			}
		}
	}
	return retained
}

func messageMetadata(row Row, existing sql.NullString) (string, error) {
	metadata := make(map[string]any)
	if existing.Valid && strings.TrimSpace(existing.String) != "" {
		if err := json.Unmarshal([]byte(existing.String), &metadata); err != nil {
			return "", fmt.Errorf("decode existing metadata: %w", err)
		}
	}
	metadata["format"] = SourceType
	metadata["file"] = row.File
	metadata["record"] = row.Record
	setEvidenceMetadata(metadata, "status", row.Status)
	setEvidenceMetadata(metadata, "delivered_date", row.DeliveredDate)
	setEvidenceMetadata(metadata, "read_date", row.ReadDate)
	setEvidenceMetadata(metadata, "replying_to", row.ReplyingTo)
	setEvidenceMetadata(metadata, "subject", row.Subject)
	setEvidenceMetadata(metadata, "text", row.Text)
	setEvidenceMetadata(metadata, "attachment", row.Attachment)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	return string(encoded), nil
}

// setEvidenceMetadata records one occurrence-evidence value exactly as the
// current row presents it, removing stale values a previous export stored.
// Occurrence reconciliation decodes these keys from archived metadata, so a
// leftover value would keep mismatching the row it belongs to.
func setEvidenceMetadata(metadata map[string]any, key, value string) {
	if value == "" {
		delete(metadata, key)
		return
	}
	metadata[key] = value
}

func conversationParticipantRefs(chat *chatPlan) []store.ConversationParticipantRef {
	keys := make([]string, 0, len(chat.participantIDs))
	for key := range chat.participantIDs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]store.ConversationParticipantRef, 0, len(keys))
	for _, key := range keys {
		result = append(result, store.ConversationParticipantRef{
			ParticipantID: chat.participantIDs[key], Role: "member",
		})
	}
	return result
}

func messageRecipients(chat *chatPlan, sender participantIdentity) ([]store.RecipientSet, []string) {
	fromID := chat.participantIDs[sender.key()]
	from := store.RecipientSet{
		Type: "from", ParticipantIDs: []int64{fromID}, DisplayNames: []string{sender.DisplayName},
	}
	keys := make([]string, 0, len(chat.participantIDs))
	for key := range chat.participantIDs {
		if key != sender.key() {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	to := store.RecipientSet{Type: "to"}
	toValues := make([]string, 0, len(keys))
	for _, key := range keys {
		identity := chat.participants[key]
		to.ParticipantIDs = append(to.ParticipantIDs, chat.participantIDs[key])
		to.DisplayNames = append(to.DisplayNames, identity.DisplayName)
		toValues = append(toValues, identity.Value)
	}
	return []store.RecipientSet{from, to}, toValues
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Field-specific domain-separated hashes retain compatibility without content.
func hashedOccurrenceFingerprint(f rowEvidenceFingerprint) rowEvidenceFingerprint {
	values := imazingidentity.EvidenceHashes([7]string{f.status, f.delivered, f.read, f.replyingTo, f.subject, f.text, f.attachment})
	return rowEvidenceFingerprint{status: values[0], delivered: values[1], read: values[2], replyingTo: values[3], subject: values[4], text: values[5], attachment: values[6]}
}

func countChatParticipants(chats map[string]*chatPlan) int {
	ids := make(map[int64]struct{})
	for _, chat := range chats {
		for _, id := range chat.participantIDs {
			ids[id] = struct{}{}
		}
	}
	return len(ids)
}
