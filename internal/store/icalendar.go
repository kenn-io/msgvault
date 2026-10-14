package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
)

// ICalendarImportSummary counts revision decisions, not generated occurrences.
type ICalendarImportSummary struct {
	SourceID                         int64
	MessageIDs                       []int64
	Changed, Unchanged, Unexpandable int
}

// icsInterpretationError distinguishes coverage failures from archive failures.
type icsInterpretationError struct{ err error }

func (e *icsInterpretationError) Error() string { return e.err.Error() }
func (e *icsInterpretationError) Unwrap() error { return e.err }

type preparedICS struct {
	component calevent.ICSComponent
	raw       []byte
	meta      calevent.ICSMetadata
}

// PersistICalendarContext imports one complete file atomically under the same
// source lock as email/calendar ingestion. Invalid files leave no event writes.
func (s *Store) PersistICalendarContext(ctx context.Context, sourceID int64, raw []byte) (ICalendarImportSummary, error) {
	summary := ICalendarImportSummary{SourceID: sourceID}
	if err := s.requireSyncSource(sourceID); err != nil {
		return summary, err
	}
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		prepared, err := s.prepareICalendarWith(ctx, tx, sourceID, raw, "", nil, "", "")
		if err != nil {
			return err
		}
		return s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary)
	})
	if err != nil {
		return ICalendarImportSummary{}, err
	}
	return summary, nil
}

func (s *Store) prepareICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, raw []byte, method string, parentMessageID *int64, parent, part string) ([]preparedICS, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	var sourceType, identifier string
	var config sql.NullString
	if err := q.QueryRow(`SELECT source_type, identifier, sync_config FROM sources WHERE id=?`, sourceID).Scan(&sourceType, &identifier, &config); err != nil {
		return nil, fmt.Errorf("read ICS source: %w", err)
	}
	var sc struct {
		TimeZone string `json:"time_zone"`
	}
	_ = json.Unmarshal([]byte(config.String), &sc)
	if sc.TimeZone == "" {
		sc.TimeZone = "UTC"
	}
	namespaceBytes, _ := json.Marshal([]string{sourceType, identifier})
	hash := sha256.Sum256(namespaceBytes)
	namespace := "source:" + hex.EncodeToString(hash[:])
	zonesByUID := make(map[string]string)
	var familyLookupErr error
	components, err := calevent.ParseICSWithContextResolver(raw, method, func(uid string) (calevent.SourceContext, error) {
		if zone, ok := zonesByUID[uid]; ok {
			return calevent.SourceContext{CalendarTimeZone: zone}, nil
		}
		zone := sc.TimeZone
		var metadata sql.NullString
		lookupErr := q.QueryRow(`SELECT m.metadata FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.source_id=? AND e.provider='ics' AND e.ical_uid=? ORDER BY e.message_id LIMIT 1`, sourceID, uid).Scan(&metadata)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			familyLookupErr = fmt.Errorf("read ICS family interpretation: %w", lookupErr)
			return calevent.SourceContext{}, familyLookupErr
		}
		if lookupErr == nil {
			var old calevent.ICSMetadata
			if json.Unmarshal([]byte(metadata.String), &old) == nil && old.TimeZone != "" {
				zone = old.TimeZone
			}
		}
		zonesByUID[uid] = zone
		return calevent.SourceContext{CalendarTimeZone: zone}, nil
	})
	if err != nil {
		if familyLookupErr != nil {
			return nil, familyLookupErr
		}
		return nil, &icsInterpretationError{err}
	}
	if len(raw) > calevent.MaxICSRetainedBytes/len(components) {
		return nil, &icsInterpretationError{errors.New("calendar retained raw exceeds 64 MiB budget")}
	}
	out := make([]preparedICS, 0, len(components))
	for index, c := range components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		zone := zonesByUID[c.Event.ICalUID]
		i := index
		out = append(out, preparedICS{component: c, raw: raw, meta: calevent.ICSMetadata{Version: 1, Index: &i, Namespace: namespace, Method: c.Method, TimeZone: zone, ParentMessageID: parentMessageID, ParentSourceMessageID: parent, PartKey: part}})
	}
	return out, nil
}

// migrateLegacyEmailICSClaimsWith replaces mutable provider-key ownership on
// existing email claims before their parent message is rekeyed.
func (s *Store) migrateLegacyEmailICSClaimsWith(
	ctx context.Context, tx *loggedTx, parentMessageID int64, oldParentSourceMessageID string,
) error {
	if oldParentSourceMessageID == "" {
		return nil
	}
	var messageType, currentSourceMessageID string
	var currentSourceID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(message_type,''),source_id,COALESCE(source_message_id,'') FROM messages WHERE id=?`, parentMessageID).
		Scan(&messageType, &currentSourceID, &currentSourceMessageID); err != nil {
		return fmt.Errorf("read email calendar claim owner: %w", err)
	}
	if !IsEmailMessageType(messageType) || currentSourceMessageID != oldParentSourceMessageID {
		return nil
	}
	encodedParent, err := json.Marshal(oldParentSourceMessageID)
	if err != nil {
		return err
	}
	pattern := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(string(encodedParent))
	pattern = "%" + pattern + "%"
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id,m.metadata
		FROM messages m
		JOIN calendar_events e ON e.message_id=m.id
		WHERE m.source_id=? AND m.message_type=? AND e.provider='ics' AND CAST(m.metadata AS TEXT) LIKE ? ESCAPE '!'
		ORDER BY m.id`, currentSourceID, calevent.MessageType, pattern)
	if err != nil {
		return fmt.Errorf("find legacy email calendar claims: %w", err)
	}
	type claim struct {
		id       int64
		metadata string
	}
	var claims []claim
	for rows.Next() {
		var candidate claim
		if err := rows.Scan(&candidate.id, &candidate.metadata); err != nil {
			_ = rows.Close()
			return err
		}
		claims = append(claims, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	q := boundQuerier{ctx: ctx, q: tx}
	for _, candidate := range claims {
		if err := ctx.Err(); err != nil {
			return err
		}
		var meta calevent.ICSMetadata
		if json.Unmarshal([]byte(candidate.metadata), &meta) != nil || meta.ParentMessageID != nil ||
			meta.ParentSourceMessageID != oldParentSourceMessageID {
			continue
		}
		_, component, _, err := readStoredICS(q, candidate.id)
		if err != nil {
			return fmt.Errorf("read legacy email calendar claim: %w", err)
		}
		parentID := parentMessageID
		meta.ParentMessageID = &parentID
		newKey := calevent.ICSMessageIDForImport(component.Event, meta)
		var conflictID int64
		err = q.QueryRow(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`, currentSourceID, newKey).Scan(&conflictID)
		if err == nil && conflictID != candidate.id {
			return errors.New("migrate legacy email calendar claim: stable identity already exists")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check stable email calendar claim identity: %w", err)
		}
		if _, err := q.Exec(`UPDATE messages SET source_message_id=? WHERE id=?`, newKey, candidate.id); err != nil {
			return fmt.Errorf("rekey legacy email calendar claim: %w", err)
		}
		if err := s.setICSMetadataWith(ctx, tx, candidate.id, meta); err != nil {
			return err
		}
	}
	return nil
}

func readStoredICS(q querier, id int64) (calevent.Event, calevent.ICSComponent, calevent.ICSMetadata, error) {
	var meta calevent.ICSMetadata
	var metadata, compression, format sql.NullString
	var stored []byte
	if err := q.QueryRow(`SELECT m.metadata,r.raw_data,r.compression,r.raw_format FROM messages m JOIN message_raw r ON r.message_id=m.id WHERE m.id=?`, id).Scan(&metadata, &stored, &compression, &format); err != nil {
		return calevent.Event{}, calevent.ICSComponent{}, meta, err
	}
	if format.String != "ics" {
		return calevent.Event{}, calevent.ICSComponent{}, meta, errors.New("calendar invitation identity conflicts with another raw format")
	}
	if err := json.Unmarshal([]byte(metadata.String), &meta); err != nil {
		return calevent.Event{}, calevent.ICSComponent{}, meta, err
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, calevent.MaxICSBytes, errMeetingRawTooLarge)
	if err != nil {
		return calevent.Event{}, calevent.ICSComponent{}, meta, err
	}
	e, ok, err := calevent.Decode("ics", raw, []byte(metadata.String), calevent.SourceContext{})
	if err != nil || !ok {
		return e, calevent.ICSComponent{}, meta, fmt.Errorf("decode prior ICS event: %w", err)
	}
	component, err := calevent.ParseICSAtIndex(raw, meta.Method, *meta.Index, calevent.SourceContext{CalendarTimeZone: meta.TimeZone})
	if err != nil {
		return e, calevent.ICSComponent{}, meta, err
	}
	return e, component, meta, nil
}

func icsRevisionNewer(c, old calevent.ICSComponent) bool {
	if c.Event.Sequence != old.Event.Sequence {
		return c.Event.Sequence > old.Event.Sequence
	}
	if !c.Stamp.Equal(old.Stamp) {
		return c.Stamp.After(old.Stamp)
	}
	return c.Event.Status == SyncStatusCancelled && old.Event.Status != SyncStatusCancelled
}

func (s *Store) persistICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if len(items) > 0 {
		if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
			return fmt.Errorf("lock ICS participant directory: %w", err)
		}
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := item.component
		// Keep email claims separate even when their first components match. A
		// later revision from another unverified email must not split a shared row
		// according to which copy is processed first.
		e := calevent.ScopeICSClaim(c.Event, item.meta)
		key := calevent.ICSMessageIDForImport(c.Event, item.meta)
		var id int64
		err := q.QueryRow(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`, sourceID, key).Scan(&id)
		exists := err == nil
		if errors.Is(err, sql.ErrNoRows) && item.meta.ParentMessageID != nil {
			legacyMeta := item.meta
			legacyMeta.ParentMessageID = nil
			legacyKeys := []string{
				calevent.ICSMessageIDForImport(c.Event, legacyMeta),
				calevent.ICSMessageID(c.Event),
			}
			for _, legacyKey := range legacyKeys {
				if legacyKey == key {
					continue
				}
				var metadata sql.NullString
				var legacyID int64
				lookupErr := q.QueryRow(`SELECT id,metadata FROM messages WHERE source_id=? AND source_message_id=?`, sourceID, legacyKey).Scan(&legacyID, &metadata)
				if errors.Is(lookupErr, sql.ErrNoRows) {
					continue
				}
				if lookupErr != nil {
					return fmt.Errorf("read legacy ICS identity: %w", lookupErr)
				}
				var existingMeta calevent.ICSMetadata
				if !metadata.Valid || json.Unmarshal([]byte(metadata.String), &existingMeta) != nil ||
					existingMeta.ParentSourceMessageID != item.meta.ParentSourceMessageID || existingMeta.PartKey != item.meta.PartKey ||
					(existingMeta.ParentMessageID != nil && *existingMeta.ParentMessageID != *item.meta.ParentMessageID) {
					continue
				}
				existingMeta.ParentMessageID = item.meta.ParentMessageID
				if _, err = q.Exec(`UPDATE messages SET source_message_id=? WHERE id=?`, key, legacyID); err != nil {
					return fmt.Errorf("adopt legacy ICS identity: %w", err)
				}
				if err = s.setICSMetadataWith(ctx, tx, legacyID, existingMeta); err != nil {
					return err
				}
				id, exists, err = legacyID, true, nil
				break
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prior ICS identity: %w", err)
		}
		if exists {
			old, revision, _, err := readStoredICS(q, id)
			if err != nil {
				return err
			}
			if !icsRevisionNewer(c, revision) {
				summary.MessageIDs = append(summary.MessageIDs, id)
				summary.Unchanged++
				continue
			}
			if e.Status == SyncStatusCancelled {
				item.meta.Inherited = &old
				if e.Kind == "single" && old.Kind == "series" {
					e.Kind, e.SeriesProviderID = "series", old.SeriesProviderID
				}
			}
			if e.Status == SyncStatusCancelled && c.Summary == "" {
				var prior sql.NullString
				if err := q.QueryRow(`SELECT subject FROM messages WHERE id=?`, id).Scan(&prior); err != nil {
					return err
				}
				c.Summary = prior.String
			}
		}
		if e.Kind == "exception" {
			masterKey := e.SeriesProviderID
			var masterID int64
			err := q.QueryRow(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`, sourceID, masterKey).Scan(&masterID)
			if err == nil {
				master, _, meta, err := readStoredICS(q, masterID)
				if err != nil {
					return err
				}
				item.meta.FamilyCancelled = master.Status == SyncStatusCancelled
				if master.Kind != "series" && master.Status == SyncStatusCancelled {
					master.Kind, master.SeriesProviderID = "series", master.ProviderEventID
					meta.Inherited = &master
					if err := s.setICSMetadataWith(ctx, tx, masterID, meta); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		item.meta.Status = e.Status
		item.meta.AllDay = e.AllDay
		metadata, err := json.Marshal(item.meta)
		if err != nil {
			return err
		}
		description, location := e.Description, e.Location
		if e.Status == SyncStatusCancelled && item.meta.Inherited != nil {
			if description == "" {
				description = item.meta.Inherited.Description
			}
			if location == "" {
				location = item.meta.Inherited.Location
			}
		}
		body := description
		if location != "" {
			body = strings.TrimSpace(body + "\nLocation: " + location)
		}
		effective := e
		if effective.Status == SyncStatusCancelled && item.meta.Inherited != nil {
			if effective.OrganizerEmail == "" {
				effective.OrganizerEmail = item.meta.Inherited.OrganizerEmail
			}
			if effective.OrganizerDisplayName == "" {
				effective.OrganizerDisplayName = item.meta.Inherited.OrganizerDisplayName
			}
			if len(effective.Attendees) == 0 {
				effective.Attendees = item.meta.Inherited.Attendees
			}
		}
		participants := make([]ParticipantPersistData, 0, 1+len(effective.Attendees))
		participantIndex := make(map[string]int, 1+len(effective.Attendees))
		addParticipant := func(email, displayName string) string {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				return ""
			}
			if index, ok := participantIndex[email]; ok {
				if participants[index].DisplayName == "" {
					participants[index].DisplayName = displayName
				}
				return email
			}
			_, domain, _ := strings.Cut(email, "@")
			participantIndex[email] = len(participants)
			participants = append(participants, ParticipantPersistData{
				EmailAddress: email,
				DisplayName:  displayName,
				Domain:       strings.ToLower(domain),
			})
			return email
		}
		organizerEmail := addParticipant(effective.OrganizerEmail, effective.OrganizerDisplayName)
		attendeeEmails := make([]string, 0, len(effective.Attendees))
		attendeeNames := make([]string, 0, len(effective.Attendees))
		seenAttendees := make(map[string]struct{}, len(effective.Attendees))
		for _, attendee := range effective.Attendees {
			email := addParticipant(attendee.Email, attendee.DisplayName)
			if email == "" {
				continue
			}
			if _, exists := seenAttendees[email]; exists {
				continue
			}
			seenAttendees[email] = struct{}{}
			attendeeEmails = append(attendeeEmails, email)
			attendeeNames = append(attendeeNames, attendee.DisplayName)
		}
		participantIDs := make(map[string]int64, len(participants))
		participantInserted := false
		for _, participant := range participants {
			participantID, err := ensureParticipantWith(q, s.dialect,
				participant.EmailAddress, participant.DisplayName, participant.Domain,
				func() error {
					participantInserted = true
					return nil
				},
			)
			if err != nil {
				return fmt.Errorf("ensure ICS participant %q: %w", participant.EmailAddress, err)
			}
			participantIDs[participant.EmailAddress] = participantID
		}
		if participantInserted {
			if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
				return fmt.Errorf("bump participant display-name revision for ICS event: %w", err)
			}
		}
		fromRecipients := RecipientSet{Type: meetingSenderRole}
		if participantID := participantIDs[organizerEmail]; participantID != 0 {
			fromRecipients.ParticipantIDs = []int64{participantID}
			fromRecipients.DisplayNames = []string{effective.OrganizerDisplayName}
			fromRecipients.EmailAddresses = []string{organizerEmail}
		}
		toRecipients := RecipientSet{
			Type:           "to",
			ParticipantIDs: make([]int64, 0, len(attendeeEmails)),
			DisplayNames:   attendeeNames,
			EmailAddresses: attendeeEmails,
		}
		for _, email := range attendeeEmails {
			toRecipients.ParticipantIDs = append(toRecipients.ParticipantIDs, participantIDs[email])
		}
		sent := sql.NullTime{}
		if e.AllDay {
			civilDate := e.StartDate
			if civilDate == "" && e.Original != nil && e.Original.AllDay && (e.Status != SyncStatusCancelled || !exists) {
				civilDate = e.Original.Date
			}
			if civilDate != "" {
				parsedDate, err := time.Parse(time.DateOnly, civilDate)
				if err != nil {
					return fmt.Errorf("parse all-day ICS start date %q: %w", civilDate, err)
				}
				sent = sql.NullTime{Time: parsedDate, Valid: true}
			}
		}
		if !sent.Valid && e.Start != nil {
			sent = sql.NullTime{Time: *e.Start, Valid: true}
		} else if !sent.Valid && e.Status == SyncStatusCancelled && exists {
			var prior sql.NullTime
			if err := q.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, id).Scan(&prior); err != nil {
				return err
			}
			sent = prior
		} else if !sent.Valid && e.Original != nil && !e.Original.AllDay {
			sent = sql.NullTime{Time: e.Original.Instant, Valid: true}
		}
		conversationKey := e.ProviderEventID
		if e.SeriesProviderID != "" {
			conversationKey = e.SeriesProviderID
		}
		conversationTitle := c.Summary
		if e.Kind == "exception" {
			conversationTitle = ""
		}
		id, err = s.persistMessageWith(ctx, tx, &MessagePersistData{
			Message:      &Message{SourceID: sourceID, SourceMessageID: key, MessageType: calevent.MessageType, Subject: sql.NullString{String: c.Summary, Valid: c.Summary != ""}, SentAt: sent, SenderID: sql.NullInt64{Int64: participantIDs[organizerEmail], Valid: participantIDs[organizerEmail] != 0}, Snippet: sql.NullString{String: body, Valid: body != ""}, SizeEstimate: int64(len(item.raw))},
			Conversation: &ConversationPersistData{SourceConversationID: conversationKey, ConversationType: "calendar", Title: conversationTitle},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, RawMIME: item.raw, RawFormat: "ics", Metadata: &sql.NullString{String: string(metadata), Valid: true}, PreserveLabels: true,
			Recipients: []RecipientSet{fromRecipients, toRecipients},
			FTS:        &FTSDoc{Subject: c.Summary, Body: body, FromAddr: organizerEmail, ToAddrs: strings.Join(attendeeEmails, " ")},
		})
		if err != nil {
			return fmt.Errorf("persist ICS event: %w", err)
		}
		if e.Kind != "exception" && e.Status != SyncStatusCancelled && c.Summary == "" {
			// General conversation upserts retain titles when the incoming title is
			// blank. An active standalone event or series master is authoritative,
			// so remove its stale summary from the shared conversation explicitly.
			if _, err := q.Exec(fmt.Sprintf(`
				UPDATE conversations SET title=NULL, updated_at=%s
				WHERE id=(SELECT conversation_id FROM messages WHERE id=?)
				  AND COALESCE(title,'') <> ''
			`, s.dialect.Now()), id); err != nil {
				return fmt.Errorf("clear stale ICS conversation title: %w", err)
			}
		}
		summary.MessageIDs = append(summary.MessageIDs, id)
		summary.Changed++
		var expansionError string
		if err := q.QueryRow(`SELECT recurrence_error FROM calendar_events WHERE message_id=?`, id).Scan(&expansionError); err != nil {
			return err
		}
		if expansionError != "" {
			summary.Unexpandable++
		}
		if e.Kind != "exception" {
			if err := s.setICSFamilyCancelledWith(ctx, tx, sourceID, e.ICalUID, item.meta, e.Status == SyncStatusCancelled); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) setICSMetadataWith(ctx context.Context, tx *loggedTx, id int64, meta calevent.ICSMetadata) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := setMessageMetadataWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, id, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return err
	}
	return s.refreshCalendarEventProjectionWith(ctx, tx, id)
}

func sameICSClaim(left, right calevent.ICSMetadata) bool {
	if left.ParentMessageID != nil && right.ParentMessageID != nil {
		return *left.ParentMessageID == *right.ParentMessageID && left.PartKey == right.PartKey
	}
	return left.ParentSourceMessageID == right.ParentSourceMessageID && left.PartKey == right.PartKey
}

func (s *Store) setICSFamilyCancelledWith(ctx context.Context, tx *loggedTx, sourceID int64, iCalUID string, claim calevent.ICSMetadata, cancelled bool) error {
	var after int64
	haveCursor := false
	for {
		query := `SELECT e.message_id,m.metadata FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.source_id=? AND e.provider='ics' AND e.ical_uid=? AND e.kind='exception'`
		args := []any{sourceID, iCalUID}
		if haveCursor {
			query += ` AND e.message_id>?`
			args = append(args, after)
		}
		query += ` ORDER BY e.message_id LIMIT 100`
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		type child struct {
			id       int64
			metadata string
		}
		var children []child
		for rows.Next() {
			var c child
			if err := rows.Scan(&c.id, &c.metadata); err != nil {
				_ = rows.Close()
				return err
			}
			children = append(children, c)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(children) == 0 {
			return nil
		}
		for _, c := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			var meta calevent.ICSMetadata
			if err := json.Unmarshal([]byte(c.metadata), &meta); err != nil {
				return err
			}
			if !sameICSClaim(meta, claim) {
				continue
			}
			if meta.FamilyCancelled != cancelled {
				meta.FamilyCancelled = cancelled
				if err := s.setICSMetadataWith(ctx, tx, c.id, meta); err != nil {
					return err
				}
			}
		}
		after = children[len(children)-1].id
		haveCursor = true
	}
}
