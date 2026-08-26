package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

const maxPersonSweepEvidenceRows = 2_000

func (s *Store) LoadPersonSweepWindow(ctx context.Context, request peoplesweep.WindowRequest) (peoplesweep.PersonWindow, error) {
	if request.PersonID <= 0 || !personSweepEvidenceLane(request.Lane) {
		return peoplesweep.PersonWindow{}, errors.New("load person sweep window: invalid person or lane")
	}
	if request.Mode != "" && request.Mode != peoplesweep.GenerationCursorOptimistic &&
		request.Mode != peoplesweep.GenerationCursorReconciliation &&
		request.Mode != peoplesweep.GenerationCursorBackstop {
		return peoplesweep.PersonWindow{}, errors.New("load person sweep window: invalid cursor mode")
	}
	if request.DocumentAfter != "" && request.Lane != peoplesweep.SourceDocumentText {
		return peoplesweep.PersonWindow{}, errors.New("load person sweep window: document continuation requires document text")
	}
	limit := request.Limit
	if limit <= 0 || limit > maxPersonSweepEvidenceRows {
		limit = maxPersonSweepEvidenceRows
	}
	window := peoplesweep.PersonWindow{NextSequence: request.AfterSequence,
		NextReconcileKey: request.ReconcileAfter, NextDocumentKey: request.DocumentAfter,
		ReconciliationDone: request.ReconcileAfter == request.ReconcileUpper && request.DocumentAfter == ""}
	if request.Mode == peoplesweep.GenerationCursorBackstop {
		upper := request.BackstopUpper
		if upper == "" {
			if request.ReconcileAfter != "" {
				return window, errors.New("load person sweep window: initial backstop cannot start after a source key")
			}
			var err error
			upper, err = s.personSweepFreshSourceUpperKey(ctx, request.Lane)
			if err != nil {
				return window, err
			}
		}
		if upper == "" {
			upper = "00000000000000000000"
		}
		request.AfterSequence = 0
		request.ThroughSequence = 0
		request.ReconcileUpper = upper
		window.NextSequence = 0
		window.NextReconcileKey = request.ReconcileAfter
		window.CapturedUpperKey = upper
		window.ReconciliationDone = request.ReconcileAfter == upper && request.DocumentAfter == ""
	} else if request.BackstopUpper != "" {
		return window, errors.New("load person sweep window: captured backstop upper requires backstop mode")
	}
	loadOptimistic := request.Mode == "" || request.Mode == peoplesweep.GenerationCursorOptimistic
	loadReconciliation := request.Mode == "" || request.Mode == peoplesweep.GenerationCursorReconciliation ||
		request.Mode == peoplesweep.GenerationCursorBackstop
	var changes []peoplesweep.ArchiveChange
	var err error
	if loadOptimistic {
		changes, err = s.ScanPersonSweepChanges(ctx, request.PersonID, request.AfterSequence, maxPersonSweepChangeScan)
		if err != nil {
			return window, err
		}
	}
	messageIDs := make([]int64, 0, limit)
	seen := make(map[int64]struct{})
	for _, change := range changes {
		if request.ThroughSequence > 0 && change.Sequence > request.ThroughSequence {
			break
		}
		if change.SourceLane != request.Lane {
			window.NextSequence = change.Sequence
			continue
		}
		window.Changes = append(window.Changes, change)
		if request.Lane == peoplesweep.SourceDocumentText {
			break
		}
		window.NextSequence = change.Sequence
		if _, ok := seen[change.MessageID]; !ok && change.MessageID > 0 {
			seen[change.MessageID] = struct{}{}
			messageIDs = append(messageIDs, change.MessageID)
		}
		if len(window.Changes) >= limit {
			break
		}
	}
	switch request.Lane {
	case peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText:
		items, found, hydrateErr := s.hydratePersonSweepMessageSet(ctx, request.PersonID, request.Lane, messageIDs)
		if hydrateErr != nil {
			return window, hydrateErr
		}
		window.Seeds = append(window.Seeds, items...)
		for _, id := range messageIDs {
			if found[id] {
				continue
			}
			for _, v := range slices.Backward(window.Changes) {
				if v.MessageID == id {
					tombstones, tombstoneErr := s.personSweepTombstones(ctx, request.PersonID, v)
					if tombstoneErr != nil {
						return window, tombstoneErr
					}
					window.Seeds = append(window.Seeds, tombstones...)
					break
				}
			}
		}
	case peoplesweep.SourceDocumentText:
		if len(window.Changes) > 0 {
			items, nextDocumentKey, done, hydrateErr := s.hydratePersonSweepDocumentChange(
				ctx, request.PersonID, window.Changes[0], request.DocumentAfter, limit)
			if hydrateErr != nil {
				return window, hydrateErr
			}
			window.Seeds = append(window.Seeds, items...)
			window.NextDocumentKey = nextDocumentKey
			if done {
				window.NextSequence = window.Changes[0].Sequence
			}
		}
	case peoplesweep.SourceAttachmentCaption, peoplesweep.SourceAttachmentOCR:
		return window, peoplesweep.ErrSourceTextUnavailable
	}
	if loadReconciliation && request.ReconcileUpper != "" && len(window.Changes) == 0 &&
		request.ReconcileAfter != request.ReconcileUpper && len(window.Seeds) < limit {
		reconcileLimit := limit - len(window.Seeds)
		if request.Lane == peoplesweep.SourceDocumentText {
			reconcileLimit = 1
		}
		reconcileChanges, next, done, scanErr := s.personSweepReconcileChanges(ctx, request, reconcileLimit)
		if scanErr != nil {
			return window, scanErr
		}
		var reconcile []peoplesweep.EvidenceItem
		if request.Lane == peoplesweep.SourceDocumentText {
			if len(reconcileChanges) > 0 {
				var documentDone bool
				reconcile, window.NextDocumentKey, documentDone, err = s.hydratePersonSweepDocumentChange(
					ctx, request.PersonID, reconcileChanges[0], request.DocumentAfter, limit-len(window.Seeds))
				if !documentDone {
					next, done = request.ReconcileAfter, false
				}
			} else {
				window.NextDocumentKey = ""
			}
		} else {
			ids := make([]int64, 0, len(reconcileChanges))
			for _, change := range reconcileChanges {
				ids = append(ids, change.MessageID)
			}
			reconcile, _, err = s.hydratePersonSweepMessageSet(ctx, request.PersonID, request.Lane, ids)
		}
		if err != nil {
			return window, err
		}
		window.Seeds = appendUniqueSweepEvidence(window.Seeds, reconcile)
		window.NextReconcileKey, window.ReconciliationDone = next, done
	}
	sort.SliceStable(window.Seeds, func(i, j int) bool { return window.Seeds[i].Ref.MessageID < window.Seeds[j].Ref.MessageID })
	return window, nil
}

func (s *Store) personSweepFreshSourceUpperKey(ctx context.Context, lane peoplesweep.SourceClass) (string, error) {
	var upper string
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		upper, err = s.personSweepSourceUpperKeyTx(ctx, tx, lane)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("capture fresh %s backstop upper key: %w", lane, err)
	}
	return upper, nil
}

func (s *Store) personSweepTombstones(ctx context.Context, personID int64, change peoplesweep.ArchiveChange) ([]peoplesweep.EvidenceItem, error) {
	evidence, err := s.personSweepStoredEvidence(ctx, personID)
	if err != nil {
		return nil, err
	}
	items := make([]peoplesweep.EvidenceItem, 0)
	for _, stored := range evidence {
		if stored.Input.SourceClass != personfacts.EvidenceArchive {
			continue
		}
		ref, decodeErr := peoplesweep.DecodePersonSweepEvidenceRef(stored.Input.SourceRef)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode stored person sweep evidence %s: %w", stored.Key, decodeErr)
		}
		if !personSweepChangeMatchesRef(personID, change, ref) {
			continue
		}
		item := peoplesweep.EvidenceItem{
			Ref: ref, EvidenceKey: stored.Key, PersonID: personID, SourceClass: ref.SourceLane,
			SourceVersion: stored.Input.SourceVersion, EventTime: change.RecordedAt.UTC(),
			RecordedTime: change.RecordedAt.UTC(), Tombstone: true,
		}
		if validateErr := peoplesweep.ValidatePersonSweepEvidenceItem(item); validateErr != nil {
			return nil, fmt.Errorf("build person sweep tombstone %s: %w", stored.Key, validateErr)
		}
		items = append(items, item)
	}
	return items, nil
}

func personSweepEvidenceLane(lane peoplesweep.SourceClass) bool {
	switch lane {
	case peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText,
		peoplesweep.SourceAttachmentCaption, peoplesweep.SourceAttachmentOCR, peoplesweep.SourceDocumentText:
		return true
	default:
		return false
	}
}

func appendUniqueSweepEvidence(dst, src []peoplesweep.EvidenceItem) []peoplesweep.EvidenceItem {
	seen := make(map[string]struct{}, len(dst))
	for _, item := range dst {
		seen[sweepEvidenceItemCoordinate(item)] = struct{}{}
	}
	for _, item := range src {
		key := sweepEvidenceItemCoordinate(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, item)
	}
	return dst
}

func sweepEvidenceItemCoordinate(item peoplesweep.EvidenceItem) string {
	coordinate := sweepEvidenceCoordinate(item.Ref)
	if item.Tombstone {
		return coordinate + "/" + item.EvidenceKey + "/" + item.SourceVersion
	}
	return coordinate
}

func sweepEvidenceCoordinate(ref peoplesweep.EvidenceRef) string {
	return fmt.Sprintf("%s/%d/%d/%d/%s/%s/%d/%d", ref.SourceLane, ref.SourceID,
		ref.MessageID, ref.AttachmentID, ref.OccurrenceKey, ref.ChunkKey, ref.SpanStart, ref.SpanEnd)
}

func (s *Store) personSweepReconcileChanges(ctx context.Context, request peoplesweep.WindowRequest, limit int) ([]peoplesweep.ArchiveChange, string, bool, error) {
	after, err := parsePersonSweepNumericKey(request.ReconcileAfter)
	if err != nil {
		return nil, request.ReconcileAfter, false, err
	}
	upper, err := parsePersonSweepNumericKey(request.ReconcileUpper)
	if err != nil {
		return nil, request.ReconcileAfter, false, err
	}
	if after >= upper || limit <= 0 {
		return []peoplesweep.ArchiveChange{}, request.ReconcileUpper, true, nil
	}
	resolution, err := resolvePersonSweepScope(ctx, s, request.PersonID)
	if err != nil {
		return nil, request.ReconcileAfter, false, err
	}
	predicate, scopeArgs := personscope.MessagePredicate(resolution, "m", "c")
	if request.Lane == peoplesweep.SourceDocumentText || request.Lane == peoplesweep.SourceAttachmentCaption || request.Lane == peoplesweep.SourceAttachmentOCR {
		args := []any{after, upper}
		args = append(args, scopeArgs...)
		args = append(args, limit)
		join := "JOIN attachments a ON a.message_id=m.id"
		selectCoords := "m.source_id,m.id,a.id,''"
		laneFilter := "TRUE"
		if request.Lane == peoplesweep.SourceDocumentText {
			join += " JOIN document_occurrences o ON o.attachment_id=a.id"
			selectCoords = "o.source_id,o.message_id,o.attachment_id,o.occurrence_key"
			laneFilter = "LOWER(COALESCE(a.media_type,''))='document'"
		}
		rows, queryErr := s.db.QueryContext(ctx, s.Rebind(fmt.Sprintf(`SELECT %s FROM messages m JOIN conversations c ON c.id=m.conversation_id %s WHERE %s AND a.id>? AND a.id<=? AND %s AND (%s) ORDER BY a.id LIMIT ?`, selectCoords, join, LiveMessagesWhere("m", true), laneFilter, predicate)), args...)
		if queryErr != nil {
			return nil, request.ReconcileAfter, false, fmt.Errorf("scan person sweep attachment reconciliation: %w", queryErr)
		}
		defer func() { _ = rows.Close() }()
		changes := make([]peoplesweep.ArchiveChange, 0, limit)
		var last int64
		for rows.Next() {
			var change peoplesweep.ArchiveChange
			change.PersonID = request.PersonID
			change.SourceLane = request.Lane
			if err := rows.Scan(&change.SourceID, &change.MessageID, &change.AttachmentID, &change.OccurrenceKey); err != nil {
				return nil, "", false, err
			}
			last = change.AttachmentID
			changes = append(changes, change)
		}
		if err := rows.Err(); err != nil {
			return nil, "", false, err
		}
		next := request.ReconcileUpper
		if last > 0 {
			next = fmt.Sprintf("%020d", last)
		}
		done := len(changes) < limit || last >= upper
		if done {
			next = request.ReconcileUpper
		}
		return changes, next, done, nil
	}
	lane := "m.message_type <> 'meeting_transcript'"
	if request.Lane == peoplesweep.SourceMeetingText {
		lane = "m.message_type = 'meeting_transcript'"
	}
	if request.Lane != peoplesweep.SourceConversationText && request.Lane != peoplesweep.SourceMeetingText {
		return []peoplesweep.ArchiveChange{}, request.ReconcileUpper, true, nil
	}
	args := []any{after, upper}
	args = append(args, scopeArgs...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(fmt.Sprintf(`
		SELECT m.id FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE %s AND m.id > ? AND m.id <= ? AND %s AND (%s)
		ORDER BY m.id LIMIT ?`, LiveMessagesWhere("m", true), lane, predicate)), args...)
	if err != nil {
		return nil, request.ReconcileAfter, false, fmt.Errorf("scan person sweep reconciliation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	changes := make([]peoplesweep.ArchiveChange, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, "", false, err
		}
		changes = append(changes, peoplesweep.ArchiveChange{PersonID: request.PersonID, SourceLane: request.Lane, MessageID: id})
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, err
	}
	next := request.ReconcileUpper
	if len(changes) > 0 {
		next = fmt.Sprintf("%020d", changes[len(changes)-1].MessageID)
	}
	done := len(changes) < limit || (len(changes) > 0 && changes[len(changes)-1].MessageID >= upper)
	if done {
		next = request.ReconcileUpper
	}
	return changes, next, done, nil
}

func parsePersonSweepNumericKey(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if len(value) != 20 {
		return 0, errors.New("person sweep source key must be 20 digits")
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("person sweep source key is invalid")
	}
	return n, nil
}

func resolvePersonSweepScope(ctx context.Context, s *Store, personID int64) (personscope.Scope, error) {
	person, err := s.GetPersonContext(ctx, personID)
	if err != nil {
		return personscope.Scope{}, fmt.Errorf("resolve durable person %d: %w", personID, err)
	}
	if len(person.ParticipantIDs) == 0 {
		return personscope.Scope{}, errors.New("person has no resolved identities")
	}
	return personscope.Scope{ParticipantIDs: slices.Clone(person.ParticipantIDs),
		Directions:                    []personscope.Direction{personscope.FromPerson, personscope.ToPerson, personscope.Group},
		IncludeUnclassifiedRosterRows: true}, nil
}

func (s *Store) hydratePersonSweepMessageSet(ctx context.Context, personID int64, lane peoplesweep.SourceClass, ids []int64) ([]peoplesweep.EvidenceItem, map[int64]bool, error) {
	found := make(map[int64]bool)
	if len(ids) == 0 {
		return []peoplesweep.EvidenceItem{}, found, nil
	}
	if lane != peoplesweep.SourceConversationText && lane != peoplesweep.SourceMeetingText {
		return []peoplesweep.EvidenceItem{}, found, nil
	}
	requestedIDs := uniquePersonSweepMessageIDs(ids)
	if len(requestedIDs) > maxPersonSweepEvidenceRows {
		return nil, nil, errors.New("person sweep message hydration exceeds bound")
	}
	queryIDs := slices.Clone(requestedIDs)
	slices.Sort(queryIDs)
	resolution, err := resolvePersonSweepScope(ctx, s, personID)
	if err != nil {
		return nil, nil, err
	}
	predicate, scopeArgs := personscope.MessagePredicate(resolution, "m", "c")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(queryIDs)), ",")
	laneSQL := "m.message_type <> 'meeting_transcript'"
	if lane == peoplesweep.SourceMeetingText {
		laneSQL = "m.message_type = 'meeting_transcript'"
	}
	args := make([]any, 0, len(queryIDs)+len(scopeArgs))
	for _, id := range queryIDs {
		args = append(args, id)
	}
	args = append(args, scopeArgs...)
	rows, err := s.db.QueryContext(ctx, s.Rebind(fmt.Sprintf(`
		SELECT m.id, m.source_id, s.source_type, COALESCE(m.source_message_id, ''),
		       COALESCE(m.subject, ''), COALESCE(mb.body_text, ''), COALESCE(m.snippet, ''),
		       COALESCE(m.sent_at, m.received_at, m.internal_date, m.archived_at), m.archived_at
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		JOIN sources s ON s.id = m.source_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id IN (%s) AND %s AND %s AND (%s)
		ORDER BY m.id`, placeholders, LiveMessagesWhere("m", true), laneSQL, predicate)), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read person sweep messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type messageRow struct {
		id, sourceID                                        int64
		sourceType, sourceMessageID, subject, body, snippet string
		event, recorded                                     time.Time
	}
	messageRowsByID := make(map[int64]messageRow, len(queryIDs))
	messageIDs := make([]int64, 0, len(queryIDs))
	for rows.Next() {
		var row messageRow
		var event, recorded requiredTimestamp
		if err := rows.Scan(&row.id, &row.sourceID, &row.sourceType, &row.sourceMessageID, &row.subject, &row.body, &row.snippet, &event, &recorded); err != nil {
			return nil, nil, fmt.Errorf("scan person sweep message: %w", err)
		}
		row.event, row.recorded = event.Time.UTC(), recorded.Time.UTC()
		messageRowsByID[row.id] = row
		messageIDs = append(messageIDs, row.id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	provenance, err := s.PersonProvenanceForMessages(ctx, messageIDs, resolution)
	if err != nil {
		return nil, nil, err
	}
	items := make([]peoplesweep.EvidenceItem, 0, len(messageRowsByID))
	for _, id := range requestedIDs {
		row, ok := messageRowsByID[id]
		if !ok {
			continue
		}
		prov := provenance[row.id]
		if prov == nil {
			continue
		}
		subjectPersonID, directness := personSweepAuthorship(
			personID, prov, lane == peoplesweep.SourceConversationText,
			personSweepSourceAuthenticatesSender(row.sourceType))
		text := canonicalSweepMessageText(row.subject, row.body, row.snippet)
		if slices.Contains(prov.Roles, personscope.RoleFrom) {
			body, _ := preprocess.Preprocess("", row.body, 0, preprocess.Config{
				StripQuotes: true, StripSignatures: true, StripHTML: true,
				StripBase64: true, CollapseWhitespace: true,
			})
			text = canonicalSweepMessageText(row.subject, body, row.snippet)
		}
		if text == "" {
			continue
		}
		fullHash := sweepSHA256(text)
		excerpt := truncateSweepRunes(text, personfacts.MaxEvidenceExcerptRunes)
		item := peoplesweep.EvidenceItem{
			Ref: peoplesweep.EvidenceRef{SourceLane: lane, SourceID: row.sourceID, MessageID: row.id,
				SourceMessageID: row.sourceMessageID, SpanEnd: utf8.RuneCountInString(excerpt)},
			PersonID: personID, SubjectPersonID: subjectPersonID, SourceClass: lane,
			SourceVersion: "message/v1:" + fullHash, ContentSHA256: fullHash,
			EventTime: row.event, RecordedTime: row.recorded, Excerpt: excerpt,
			Highlight: peoplesweep.TextSpan{End: utf8.RuneCountInString(excerpt)}, Provenance: *prov,
			IdentityBasisPoints: 1000, Directness: directness, Authority: personfacts.AuthorityOrdinary,
		}
		items = append(items, item)
		found[row.id] = true
	}
	return items, found, nil
}

func (s *Store) hydratePersonSweepDocumentChange(
	ctx context.Context,
	personID int64,
	change peoplesweep.ArchiveChange,
	after string,
	limit int,
) ([]peoplesweep.EvidenceItem, string, bool, error) {
	if change.SourceLane != peoplesweep.SourceDocumentText || change.AttachmentID <= 0 || limit <= 0 {
		return []peoplesweep.EvidenceItem{}, "", true, nil
	}
	kind, position, tombstoneAfter, err := parsePersonSweepDocumentContinuation(after)
	if err != nil {
		return nil, "", false, err
	}
	if kind == "tombstone" {
		return s.pagePersonSweepDocumentTombstones(ctx, personID, change, tombstoneAfter, limit)
	}
	resolution, err := resolvePersonSweepScope(ctx, s, personID)
	if err != nil {
		return nil, "", false, err
	}
	predicate, scopeArgs := personscope.MessagePredicate(resolution, "m", "cv")
	items := make([]peoplesweep.EvidenceItem, 0)
	conditions := "a.id=?"
	args := []any{change.AttachmentID}
	if change.MessageID > 0 {
		conditions += " AND m.id=?"
		args = append(args, change.MessageID)
	}
	if change.SourceID > 0 {
		conditions += " AND o.source_id=?"
		args = append(args, change.SourceID)
	}
	if change.OccurrenceKey != "" {
		conditions += " AND o.occurrence_key=?"
		args = append(args, change.OccurrenceKey)
	}
	args = append(args, scopeArgs...)
	args = append(args, position, limit+1)
	rows, queryErr := s.db.QueryContext(ctx, s.Rebind(fmt.Sprintf(`
			SELECT o.source_id,o.message_id,o.attachment_id,COALESCE(m.source_message_id,''),s.source_type,
			       o.occurrence_key,dc.chunk_key,dc.text,dc.checksum,h.extraction_id,
			       COALESCE(m.sent_at,m.received_at,m.internal_date,h.switched_at),h.switched_at,dc.ordinal
			FROM document_extraction_heads h
			JOIN document_extraction_profiles p ON p.id=h.profile_id
			JOIN document_provider_consents consent ON consent.profile_id=p.id
			JOIN document_chunks dc ON dc.extraction_id=h.extraction_id
			JOIN document_occurrences o ON o.canonical_blob_hash=h.canonical_blob_hash
			JOIN attachments a ON a.id=o.attachment_id
			JOIN messages m ON m.id=o.message_id
			JOIN sources s ON s.id=o.source_id
			JOIN conversations cv ON cv.id=m.conversation_id
			CROSS JOIN document_index_state ds
			WHERE %s AND %s AND (%s) AND %s AND dc.ordinal >= ?
			ORDER BY dc.ordinal LIMIT ?`, documentSearchValidityForConsent("consent"), conditions, predicate, LiveMessagesWhere("m", true))), args...)
	if queryErr != nil {
		return nil, "", false, fmt.Errorf("read person sweep document changes: %w", queryErr)
	}
	defer func() { _ = rows.Close() }()
	nextPosition := position
	more := false
	scanned := 0
	for rows.Next() {
		if scanned == limit {
			more = true
			break
		}
		scanned++
		var ref peoplesweep.EvidenceRef
		ref.SourceLane = peoplesweep.SourceDocumentText
		var sourceType, text, checksum, extractionID string
		var event, recorded requiredTimestamp
		var ordinal int
		if scanErr := rows.Scan(&ref.SourceID, &ref.MessageID, &ref.AttachmentID,
			&ref.SourceMessageID, &sourceType, &ref.OccurrenceKey, &ref.ChunkKey, &text,
			&checksum, &extractionID, &event, &recorded, &ordinal); scanErr != nil {
			return nil, "", false, scanErr
		}
		nextPosition = ordinal + 1
		excerpt := truncateSweepRunes(text, personfacts.MaxEvidenceExcerptRunes)
		ref.SpanEnd = utf8.RuneCountInString(excerpt)
		provMap, provErr := s.PersonProvenanceForMessages(ctx, []int64{ref.MessageID}, resolution)
		if provErr != nil {
			return nil, "", false, provErr
		}
		prov := provMap[ref.MessageID]
		if prov == nil {
			continue
		}
		subject, direct := personSweepAuthorship(personID, prov, false,
			personSweepSourceAuthenticatesSender(sourceType))
		items = append(items, peoplesweep.EvidenceItem{Ref: ref, PersonID: personID, SubjectPersonID: subject, SourceClass: peoplesweep.SourceDocumentText, SourceVersion: "document/v1:" + extractionID + ":" + checksum, ContentSHA256: sweepSHA256(text), EventTime: event.Time.UTC(), RecordedTime: recorded.Time.UTC(), Excerpt: excerpt, Highlight: peoplesweep.TextSpan{End: ref.SpanEnd}, Provenance: *prov, IdentityBasisPoints: 1000, Directness: direct, Authority: personfacts.AuthorityOrdinary})
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, "", false, rowErr
	}
	if more {
		return items, fmt.Sprintf("live:%020d", nextPosition), false, nil
	}
	if scanned > 0 || kind == "live" {
		return items, "", true, nil
	}
	return s.pagePersonSweepDocumentTombstones(ctx, personID, change, "", limit)
}

func parsePersonSweepDocumentContinuation(value string) (string, int, string, error) {
	if value == "" {
		return "", 0, "", nil
	}
	if after, ok := strings.CutPrefix(value, "tombstone:"); ok {
		key := after
		if key == "" {
			return "", 0, "", errors.New("load person sweep window: invalid tombstone document continuation")
		}
		return "tombstone", 0, key, nil
	}
	if !strings.HasPrefix(value, "live:") {
		return "", 0, "", errors.New("load person sweep window: invalid document continuation")
	}
	position, err := strconv.Atoi(strings.TrimPrefix(value, "live:"))
	if err != nil || position <= 0 {
		return "", 0, "", errors.New("load person sweep window: invalid live document continuation")
	}
	return "live", position, "", nil
}

func (s *Store) pagePersonSweepDocumentTombstones(
	ctx context.Context,
	personID int64,
	change peoplesweep.ArchiveChange,
	afterKey string,
	limit int,
) ([]peoplesweep.EvidenceItem, string, bool, error) {
	tombstones, err := s.personSweepTombstones(ctx, personID, change)
	if err != nil {
		return nil, "", false, err
	}
	sort.Slice(tombstones, func(i, j int) bool { return tombstones[i].EvidenceKey < tombstones[j].EvidenceKey })
	// Tombstone continuations retain the full evidence key because stored
	// document versions for one chunk can have distinct evidence identities.
	// The prefix distinguishes this stable key page from a live chunk ordinal.
	start := sort.Search(len(tombstones), func(index int) bool {
		return tombstones[index].EvidenceKey > afterKey
	})
	tombstones = tombstones[start:]
	if len(tombstones) <= limit {
		return tombstones, "", true, nil
	}
	page := tombstones[:limit]
	return page, "tombstone:" + page[len(page)-1].EvidenceKey, false, nil
}

func canonicalSweepMessageText(subject, body, snippet string) string {
	subject, body, snippet = strings.TrimSpace(subject), strings.TrimSpace(body), strings.TrimSpace(snippet)
	if subject != "" && body != "" {
		return subject + "\n\n" + body
	}
	if body != "" {
		return body
	}
	if snippet != "" {
		return snippet
	}
	return subject
}

func truncateSweepRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
func sweepSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Store) ListPersonSweepHistoricalCandidates(
	ctx context.Context, request peoplesweep.HistoricalCandidateRequest,
) ([]int64, error) {
	if request.Limit <= 0 {
		return []int64{}, nil
	}
	if request.Limit > maxPersonSweepEvidenceRows {
		request.Limit = maxPersonSweepEvidenceRows
	}
	resolution, err := resolvePersonSweepScope(ctx, s, request.PersonID)
	if err != nil {
		return nil, err
	}
	after, before, err := sweepDateBounds(request.SourceSince, request.SourceUntil)
	if err != nil {
		return nil, err
	}
	predicate, args := personscope.MessagePredicate(resolution, "m", "c")
	lanePredicate := personSweepHistoricalLanePredicate(request.SourceClasses)
	eventTime := "COALESCE(m.sent_at,m.received_at,m.internal_date,m.archived_at)"
	datePredicate := ""
	if after != nil {
		datePredicate += " AND " + eventTime + " >= ?"
		args = append(args, *after)
	}
	if before != nil {
		datePredicate += " AND " + eventTime + " < ?"
		args = append(args, *before)
	}
	args = append(args, request.Limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(fmt.Sprintf(`SELECT m.id FROM messages m
		JOIN conversations c ON c.id=m.conversation_id WHERE %s AND (%s) AND (%s)%s
		ORDER BY %s DESC,m.id DESC LIMIT ?`, LiveMessagesWhere("m", true), predicate,
		lanePredicate, datePredicate, eventTime)), args...)
	if err != nil {
		return nil, fmt.Errorf("list person sweep historical candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, request.Limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func personSweepHistoricalLanePredicate(classes []peoplesweep.SourceClass) string {
	if len(classes) == 0 {
		return "TRUE"
	}
	clauses := make([]string, 0, 3)
	if sweepSourceAllowed(classes, peoplesweep.SourceConversationText) {
		clauses = append(clauses, "m.message_type <> 'meeting_transcript'")
	}
	if sweepSourceAllowed(classes, peoplesweep.SourceMeetingText) {
		clauses = append(clauses, "m.message_type = 'meeting_transcript'")
	}
	if sweepSourceAllowed(classes, peoplesweep.SourceDocumentText) {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM document_occurrences occurrence
			JOIN document_extraction_heads head
			  ON head.canonical_blob_hash = occurrence.canonical_blob_hash
			JOIN document_chunks chunk ON chunk.extraction_id = head.extraction_id
			WHERE occurrence.message_id = m.id
		)`)
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return strings.Join(clauses, " OR ")
}

func (s *Store) HydratePersonSweepMessages(ctx context.Context, personID int64, messageIDs []int64) ([]peoplesweep.EvidenceItem, error) {
	conversation, foundConversation, err := s.hydratePersonSweepMessageSet(ctx, personID, peoplesweep.SourceConversationText, messageIDs)
	if err != nil {
		return nil, err
	}
	meeting, foundMeeting, err := s.hydratePersonSweepMessageSet(ctx, personID, peoplesweep.SourceMeetingText, messageIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range messageIDs {
		if !foundConversation[id] && !foundMeeting[id] {
			return nil, fmt.Errorf("message %d is outside person sweep scope or lacks durable text", id)
		}
	}
	items := append(slices.Clone(conversation), meeting...)
	sortPersonSweepItemsByMessageOrder(items, messageIDs)
	return items, nil
}

func (s *Store) SearchPersonSweepMessages(ctx context.Context, request peoplesweep.ContextRequest) ([]peoplesweep.EvidenceItem, error) {
	if !sweepSourceAllowed(request.SourceClasses, peoplesweep.SourceConversationText) && !sweepSourceAllowed(request.SourceClasses, peoplesweep.SourceMeetingText) {
		return []peoplesweep.EvidenceItem{}, nil
	}
	ids := request.CandidateMessageIDs
	if len(ids) == 0 {
		var err error
		ids, err = s.ListPersonSweepHistoricalCandidates(ctx, peoplesweep.HistoricalCandidateRequest{
			PersonID: request.PersonID, SourceClasses: request.SourceClasses,
			SourceSince: request.SourceSince, SourceUntil: request.SourceUntil,
			Limit: maxPersonSweepEvidenceRows,
		})
		if err != nil {
			return nil, err
		}
	}
	conversation, _, err := s.hydratePersonSweepMessageSet(ctx, request.PersonID, peoplesweep.SourceConversationText, ids)
	if err != nil {
		return nil, err
	}
	meeting, _, err := s.hydratePersonSweepMessageSet(ctx, request.PersonID, peoplesweep.SourceMeetingText, ids)
	if err != nil {
		return nil, err
	}
	items := append(slices.Clone(conversation), meeting...)
	sortPersonSweepItemsByMessageOrder(items, ids)
	after, before, err := sweepDateBounds(request.SourceSince, request.SourceUntil)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(request.Target.Description + " " + request.Target.Slug + " " + request.Target.Key)))
	out := make([]peoplesweep.EvidenceItem, 0)
	for _, item := range items {
		if !sweepSourceAllowed(request.SourceClasses, item.SourceClass) || after != nil && item.EventTime.Before(*after) || before != nil && !item.EventTime.Before(*before) {
			continue
		}
		lower := strings.ToLower(item.Excerpt)
		matched := len(terms) == 0
		for _, term := range terms {
			if strings.Contains(lower, term) {
				matched = true
				break
			}
		}
		if matched {
			out = append(out, item)
		}
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sweepSourceAllowed(classes []peoplesweep.SourceClass, lane peoplesweep.SourceClass) bool {
	if len(classes) == 0 {
		return true
	}
	return slices.Contains(classes, lane)
}

func uniquePersonSweepMessageIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

func sortPersonSweepItemsByMessageOrder(items []peoplesweep.EvidenceItem, ids []int64) {
	rank := make(map[int64]int, len(ids))
	for index, id := range ids {
		if _, exists := rank[id]; !exists {
			rank[id] = index
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return rank[items[i].Ref.MessageID] < rank[items[j].Ref.MessageID]
	})
}
func sweepDateBounds(since, until string) (*time.Time, *time.Time, error) {
	var after, before *time.Time
	if since != "" {
		v, e := time.Parse("2006-01-02", since)
		if e != nil {
			return nil, nil, fmt.Errorf("parse person sweep source since %q: %w", since, e)
		}
		v = v.UTC()
		after = &v
	}
	if until != "" {
		v, e := time.Parse("2006-01-02", until)
		if e != nil {
			return nil, nil, fmt.Errorf("parse person sweep source until %q: %w", until, e)
		}
		v = v.UTC().AddDate(0, 0, 1)
		before = &v
	}
	if after != nil && before != nil && !after.Before(*before) {
		return nil, nil, errors.New("invalid person sweep date bounds")
	}
	return after, before, nil
}

func (s *Store) SearchPersonSweepDocuments(ctx context.Context, request peoplesweep.DocumentContextRequest) ([]peoplesweep.EvidenceItem, error) {
	resolution, err := resolvePersonSweepScope(ctx, s, request.PersonID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Query) == "" {
		return []peoplesweep.EvidenceItem{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 20
	}
	response, err := s.SearchDocuments(ctx, DocumentSearchRequest{
		Query: request.Query, MessageIDs: request.CandidateMessageIDs,
		PageSize: min(limit, 100), After: request.After, Before: request.Before,
		PersonID: request.PersonID, Person: &resolution,
	})
	if err != nil {
		return nil, err
	}
	items := make([]peoplesweep.EvidenceItem, 0, len(response.Results))
	sourceAuthentication := make(map[int64]bool)
	for _, result := range response.Results {
		text, checksum, recorded, loadErr := s.loadCurrentDocumentChunk(ctx, result.ExtractionID, result.ChunkKey)
		if loadErr != nil {
			return nil, loadErr
		}
		event := recorded
		if result.OccurredAt != nil {
			event = result.OccurredAt.UTC()
		}
		prov := personscope.Provenance{}
		if result.PersonProvenance != nil {
			prov = *result.PersonProvenance
		}
		authenticated, loaded := sourceAuthentication[result.SourceID]
		if !loaded {
			source, sourceErr := s.GetSourceByIDContext(ctx, result.SourceID)
			if sourceErr != nil {
				return nil, sourceErr
			}
			authenticated = personSweepSourceAuthenticatesSender(source.SourceType)
			sourceAuthentication[result.SourceID] = authenticated
		}
		subject, direct := personSweepAuthorship(request.PersonID, &prov, false, authenticated)
		items = append(items, peoplesweep.EvidenceItem{Ref: peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceDocumentText, SourceID: result.SourceID, MessageID: result.MessageID, AttachmentID: result.AttachmentID, SourceMessageID: result.SourceMessageID, OccurrenceKey: result.OccurrenceKey, ChunkKey: result.ChunkKey, SpanStart: result.HighlightStart, SpanEnd: result.HighlightEnd}, PersonID: request.PersonID, SubjectPersonID: subject, SourceClass: peoplesweep.SourceDocumentText, SourceVersion: "document/v1:" + result.ExtractionID + ":" + checksum, ContentSHA256: sweepSHA256(text), EventTime: event, RecordedTime: recorded, Excerpt: result.Excerpt, Highlight: peoplesweep.TextSpan{Start: result.HighlightStart, End: result.HighlightEnd}, Provenance: prov, IdentityBasisPoints: 1000, Directness: direct, Authority: personfacts.AuthorityOrdinary})
	}
	return items, nil
}

func (s *Store) loadCurrentDocumentChunk(ctx context.Context, extractionID, chunkKey string) (string, string, time.Time, error) {
	var text, checksum string
	var recorded requiredTimestamp
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT dc.text,dc.checksum,h.switched_at FROM document_chunks dc JOIN document_extraction_heads h ON h.extraction_id=dc.extraction_id WHERE dc.extraction_id=? AND dc.chunk_key=?`), extractionID, chunkKey).Scan(&text, &checksum, &recorded)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("load person sweep document chunk: %w", err)
	}
	return text, checksum, recorded.Time.UTC(), nil
}

type PersonSweepEvidenceAligner struct{ Store *Store }

func (a PersonSweepEvidenceAligner) Align(ctx context.Context, input personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	reject := func(reason personfacts.DecisionReason, detail string) personfacts.AlignmentResult {
		return personfacts.AlignmentResult{Failure: &personfacts.ValidationFailure{Action: personfacts.DecisionIdentityRejected, Reason: reason, Detail: detail}}
	}
	if input.SourceClass != personfacts.EvidenceArchive {
		return reject(personfacts.ReasonUnalignedEvidence, "person sweep evidence must use archive class"), nil
	}
	ref, decodeErr := peoplesweep.DecodePersonSweepEvidenceRef(input.SourceRef)
	if decodeErr != nil {
		return reject(personfacts.ReasonUnalignedEvidence, "invalid person sweep evidence ref"), nil //nolint:nilerr // Malformed evidence is a recorded domain rejection.
	}
	if ref.SourceLane == peoplesweep.SourceAttachmentCaption || ref.SourceLane == peoplesweep.SourceAttachmentOCR {
		return personfacts.AlignmentResult{}, peoplesweep.ErrSourceTextUnavailable
	}
	if a.Store == nil {
		return personfacts.AlignmentResult{}, errors.New("person sweep evidence aligner has no store")
	}
	var err error
	var current peoplesweep.EvidenceItem
	if ref.SourceLane == peoplesweep.SourceDocumentText {
		current, err = a.Store.alignDocumentItem(ctx, input.PersonID, ref, input.Excerpt)
	} else {
		items, _, loadErr := a.Store.hydratePersonSweepMessageSet(ctx, input.PersonID, ref.SourceLane, []int64{ref.MessageID})
		err = loadErr
		if err == nil && len(items) == 1 {
			current = items[0]
		} else if err == nil {
			err = sql.ErrNoRows
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return reject(personfacts.ReasonUnalignedEvidence, "person sweep source is unavailable"), nil
	}
	if err != nil {
		return personfacts.AlignmentResult{}, err
	}
	if current.Ref.SourceLane != ref.SourceLane || current.Ref.SourceID != ref.SourceID || current.Ref.MessageID != ref.MessageID || current.Ref.AttachmentID != ref.AttachmentID || current.Ref.SourceMessageID != ref.SourceMessageID || current.Ref.OccurrenceKey != ref.OccurrenceKey || current.Ref.ChunkKey != ref.ChunkKey || current.Ref.SpanStart != ref.SpanStart || current.Ref.SpanEnd != ref.SpanEnd {
		return reject(personfacts.ReasonUnalignedEvidence, "person sweep source coordinates changed"), nil
	}
	if !samePersonSweepSubject(input.SubjectPersonID, current.SubjectPersonID) || current.SubjectPersonID == nil || *current.SubjectPersonID != current.PersonID {
		return reject(personfacts.ReasonIdentityMismatch, "evidence subject does not match canonical person"), nil
	}
	start, end := int64(current.Highlight.Start), int64(current.Highlight.End)
	if input.SourceVersion != current.SourceVersion || input.ContentSHA256 != current.ContentSHA256 || input.Excerpt != current.Excerpt || input.SpanStart == nil || input.SpanEnd == nil || *input.SpanStart != start || *input.SpanEnd != end || !input.EventTime.Equal(current.EventTime) || !input.RecordedTime.Equal(current.RecordedTime) || input.IdentityScore != current.IdentityBasisPoints || input.Directness != current.Directness || input.Authority != current.Authority {
		return reject(personfacts.ReasonUnalignedEvidence, "person sweep evidence no longer aligns"), nil
	}
	return personfacts.AlignmentResult{Accepted: true, SourceVersion: current.SourceVersion, ContentSHA256: current.ContentSHA256}, nil
}

func (s *Store) alignDocumentItem(ctx context.Context, personID int64, ref peoplesweep.EvidenceRef, excerpt string) (peoplesweep.EvidenceItem, error) {
	resolution, err := resolvePersonSweepScope(ctx, s, personID)
	if err != nil {
		return peoplesweep.EvidenceItem{}, err
	}
	predicate, args := personscope.MessagePredicate(resolution, "m", "c")
	args = append([]any{ref.AttachmentID, ref.MessageID, ref.SourceID, ref.OccurrenceKey, ref.ChunkKey}, args...)
	var text, checksum, extractionID, sourceMessageID, sourceType string
	var event, recorded requiredTimestamp
	err = s.db.QueryRowContext(ctx, s.Rebind(fmt.Sprintf(`SELECT dc.text,dc.checksum,h.extraction_id,COALESCE(m.source_message_id,''),s.source_type,COALESCE(m.sent_at,m.received_at,m.internal_date,h.switched_at),h.switched_at FROM document_chunks dc JOIN document_extraction_heads h ON h.extraction_id=dc.extraction_id JOIN document_occurrences o ON o.canonical_blob_hash=h.canonical_blob_hash JOIN attachments a ON a.id=o.attachment_id JOIN messages m ON m.id=o.message_id JOIN sources s ON s.id=m.source_id JOIN conversations c ON c.id=m.conversation_id WHERE a.id=? AND m.id=? AND m.source_id=? AND o.occurrence_key=? AND dc.chunk_key=? AND %s AND (%s)`, LiveMessagesWhere("m", true), predicate)), args...).Scan(&text, &checksum, &extractionID, &sourceMessageID, &sourceType, &event, &recorded)
	if err != nil {
		return peoplesweep.EvidenceItem{}, err
	}
	if !strings.Contains(text, excerpt) || ref.SpanEnd > utf8.RuneCountInString(excerpt) {
		return peoplesweep.EvidenceItem{}, sql.ErrNoRows
	}
	provMap, err := s.PersonProvenanceForMessages(ctx, []int64{ref.MessageID}, resolution)
	if err != nil {
		return peoplesweep.EvidenceItem{}, err
	}
	prov := provMap[ref.MessageID]
	if prov == nil {
		return peoplesweep.EvidenceItem{}, sql.ErrNoRows
	}
	subject, direct := personSweepAuthorship(personID, prov, false,
		personSweepSourceAuthenticatesSender(sourceType))
	actualRef := ref
	actualRef.SourceMessageID = sourceMessageID
	return peoplesweep.EvidenceItem{Ref: actualRef, PersonID: personID, SubjectPersonID: subject, SourceClass: peoplesweep.SourceDocumentText, SourceVersion: "document/v1:" + extractionID + ":" + checksum, ContentSHA256: sweepSHA256(text), EventTime: event.Time.UTC(), RecordedTime: recorded.Time.UTC(), Excerpt: excerpt, Highlight: peoplesweep.TextSpan{Start: ref.SpanStart, End: ref.SpanEnd}, Provenance: *prov, IdentityBasisPoints: 1000, Directness: direct, Authority: personfacts.AuthorityOrdinary}, nil
}

func personSweepAuthorship(
	personID int64, provenance *personscope.Provenance, attributedMessageText, senderAuthenticated bool,
) (*int64, personfacts.EvidenceDirectness) {
	if provenance != nil && slices.Contains(provenance.Roles, personscope.RoleFrom) && senderAuthenticated {
		subject := personID
		if attributedMessageText {
			return &subject, personfacts.DirectSelf
		}
		return &subject, personfacts.DirectOther
	}
	return nil, personfacts.DirectOther
}

func personSweepSourceAuthenticatesSender(sourceType string) bool {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "apple_messages", "beeper", "discord", "facebook_messenger", "google_messages",
		"imessage", "slack", "synctech-sms", "synctech_sms", "teams", "whatsapp":
		return true
	default:
		return false
	}
}

func samePersonSweepSubject(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func (s *Store) BuildPersonSweepEvidenceStatusChanges(ctx context.Context, personID int64, changes []peoplesweep.ArchiveChange) ([]personfacts.EvidenceStatusChange, error) {
	evidence, err := s.personSweepStoredEvidence(ctx, personID)
	if err != nil {
		return nil, err
	}
	type evidenceVersion struct {
		key, version string
	}
	terminal := map[evidenceVersion]personfacts.EvidenceStatusChange{}
	for _, stored := range evidence {
		if stored.Input.SourceClass != personfacts.EvidenceArchive {
			continue
		}
		ref, err := peoplesweep.DecodePersonSweepEvidenceRef(stored.Input.SourceRef)
		if err != nil {
			return nil, fmt.Errorf("decode stored person sweep evidence %s: %w", stored.Key, err)
		}
		for _, change := range changes {
			if !personSweepChangeMatchesRef(personID, change, ref) {
				continue
			}
			supported, reason, ok := personSweepStatusEffect(change.EvidenceEffect)
			if ok {
				key := evidenceVersion{stored.Key, stored.Input.SourceVersion}
				if supported {
					alignment, alignErr := (PersonSweepEvidenceAligner{Store: s}).Align(ctx, stored.Input)
					if errors.Is(alignErr, peoplesweep.ErrSourceTextUnavailable) {
						continue
					}
					if alignErr != nil {
						return nil, fmt.Errorf("verify reactivated person sweep evidence %s: %w", stored.Key, alignErr)
					}
					if !alignment.Accepted {
						continue
					}
				}
				terminal[key] = personfacts.EvidenceStatusChange{
					EvidenceKey: key.key, SourceVersion: key.version,
					Supported: supported, Reason: reason,
				}
			}
		}
	}
	out := make([]personfacts.EvidenceStatusChange, 0, len(terminal))
	for _, change := range terminal {
		out = append(out, change)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EvidenceKey != out[j].EvidenceKey {
			return out[i].EvidenceKey < out[j].EvidenceKey
		}
		if out[i].SourceVersion != out[j].SourceVersion {
			return out[i].SourceVersion < out[j].SourceVersion
		}
		if out[i].Supported != out[j].Supported {
			return !out[i].Supported
		}
		return out[i].Reason < out[j].Reason
	})
	return out, nil
}

func (s *Store) personSweepStoredEvidence(ctx context.Context, personID int64) ([]personfacts.Evidence, error) {
	evidence := make([]personfacts.Evidence, 0)
	for offset := 0; ; offset += 200 {
		page, err := s.ListPersonFactEvidenceContext(ctx, personID, personfacts.EvidenceFilter{Limit: 200, Offset: offset})
		if err != nil {
			return nil, err
		}
		evidence = append(evidence, page...)
		if len(page) < 200 {
			break
		}
	}
	return evidence, nil
}

func personSweepChangeMatchesRef(personID int64, change peoplesweep.ArchiveChange, ref peoplesweep.EvidenceRef) bool {
	if change.PersonID != personID || change.SourceLane != ref.SourceLane ||
		change.SourceID != ref.SourceID || change.MessageID != ref.MessageID {
		return false
	}
	switch ref.SourceLane {
	case peoplesweep.SourceDocumentText:
		return change.AttachmentID == ref.AttachmentID &&
			change.OccurrenceKey == ref.OccurrenceKey
	case peoplesweep.SourceAttachmentCaption, peoplesweep.SourceAttachmentOCR:
		return change.AttachmentID == ref.AttachmentID
	default:
		return change.AttachmentID == 0 && change.OccurrenceKey == ""
	}
}
func personSweepStatusEffect(effect peoplesweep.EvidenceChangeEffect) (bool, personfacts.EvidenceStatusReason, bool) {
	switch effect {
	case peoplesweep.EvidenceEffectSourceDeleted:
		return false, personfacts.EvidenceStatusSourceDeleted, true
	case peoplesweep.EvidenceEffectSourceEdited:
		return false, personfacts.EvidenceStatusSourceEdited, true
	case peoplesweep.EvidenceEffectScopeUnlinked:
		return false, personfacts.EvidenceStatusScopeUnlinked, true
	case peoplesweep.EvidenceEffectIdentityReassigned:
		return false, personfacts.EvidenceStatusIdentityReassigned, true
	case peoplesweep.EvidenceEffectSourceReimported:
		return true, personfacts.EvidenceStatusSourceReimported, true
	case peoplesweep.EvidenceEffectScopeRelinked:
		return true, personfacts.EvidenceStatusScopeRelinked, true
	default:
		return false, "", false
	}
}
