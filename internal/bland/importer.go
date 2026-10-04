package bland

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

type Source interface {
	ListCalls(ctx context.Context, options ListOptions) (*Page, error)
	GetCall(ctx context.Context, id string) (*Call, error)
	GetPostCall(ctx context.Context, id string) (jsontext.Value, error)
	GetCorrectedTranscript(ctx context.Context, id string) (jsontext.Value, error)
	OpenRecording(ctx context.Context, id string) (*Recording, error)
}
type Importer struct {
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(s *store.Store, c Source) *Importer {
	return &Importer{store: s, client: c, now: time.Now}
}

type ImportOptions struct {
	Identifier, AccountEmail, AttachmentsDir string
	Full                                     bool
	FetchCorrectedTranscript                 bool
	Limit                                    int
	CreatedAfter                             time.Time
	MediaPolicy                              attachmentpolicy.Policy
	Progress                                 func(string)
}
type ImportSummary struct {
	SourceID, MeetingsProcessed, MeetingsAdded, MeetingsUpdated, MaintenanceRetries, Skipped, Errors int64
	PartialCoverage                                                                                  bool
}
type pendingCall struct {
	Discovered    time.Time `json:"discovered"`
	Next          time.Time `json:"next"`
	Until         time.Time `json:"until"`
	Terminal      bool      `json:"terminal"`
	RetryDeferred bool      `json:"retry_deferred,omitempty"`
	Parent        string    `json:"parent,omitempty"`
}
type syncState struct {
	Version      int                    `json:"version"`
	Watermark    string                 `json:"watermark,omitempty"`
	WindowStart  string                 `json:"window_start,omitempty"`
	WindowEnd    string                 `json:"window_end,omitempty"`
	CreatedAfter string                 `json:"created_after,omitempty"`
	Full         bool                   `json:"full"`
	Seen         map[string]bool        `json:"seen,omitempty"`
	Pending      map[string]pendingCall `json:"pending"`
}

func (s *syncState) validate() error {
	if s.Version != 1 {
		return errors.New("unsupported Bland cursor version")
	}
	for _, d := range []string{s.Watermark, s.WindowStart, s.WindowEnd, s.CreatedAfter} {
		if d != "" {
			if _, err := time.Parse(time.DateOnly, d); err != nil {
				return errors.New("invalid Bland cursor date")
			}
		}
	}
	for id, p := range s.Pending {
		if !validID(id) || p.Discovered.IsZero() || p.Until.IsZero() {
			return errors.New("invalid Bland pending cursor")
		}
	}
	return nil
}
func (imp *Importer) Import(ctx context.Context, o ImportOptions) (sum *ImportSummary, retErr error) {
	if o.Limit < 0 {
		return nil, errors.New("limit must be zero or positive")
	}
	if err := o.MediaPolicy.Validate(); err != nil {
		return nil, err
	}
	src, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, o.Identifier)
	if err != nil {
		return nil, err
	}
	sum = &ImportSummary{SourceID: src.ID}
	state := syncState{Version: 1, Pending: map[string]pendingCall{}}
	if src.SyncCursor.Valid && src.SyncCursor.String != "" {
		if err := json.Unmarshal([]byte(src.SyncCursor.String), &state); err != nil {
			return sum, err
		}
		if err := state.validate(); err != nil {
			return sum, err
		}
	}
	if resumed, resumeErr := imp.store.GetLatestCheckpointedSyncByType(src.ID, SourceType); resumeErr == nil && resumed.CursorBefore.Valid {
		if err := json.Unmarshal([]byte(resumed.CursorBefore.String), &state); err != nil {
			return sum, err
		}
		if err := state.validate(); err != nil {
			return sum, err
		}
	} else if resumeErr != nil && !errors.Is(resumeErr, store.ErrSyncRunNotFound) {
		return sum, resumeErr
	}
	if state.Pending == nil {
		state.Pending = map[string]pendingCall{}
	}
	planned := int64(len(state.Pending))
	if o.Full {
		archivedCount, err := imp.store.CountMessagesForSourceContext(ctx, src.ID)
		if err != nil {
			return sum, err
		}
		planned += archivedCount
	}
	if err := imp.store.AddAccountIdentityContext(ctx, src.ID, o.AccountEmail, "account-email"); err != nil {
		return sum, err
	}
	syncID, err := imp.store.StartSync(src.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(src.ID, syncID)
	imp = &scoped
	now := imp.now().UTC()
	// Bound full-state snapshot volume, including runs with no list total.
	// Final/failure checkpoints always retain the entire current state.
	const maxIntermediateCheckpoints = 16
	checkpointCount := 0
	processedCount := int64(0)
	nextCheckpoint := max(int64(1), (planned+maxIntermediateCheckpoints-1)/maxIntermediateCheckpoints)
	save := func(force ...bool) error {
		forced := len(force) > 0 && force[0]
		if !forced && (checkpointCount >= maxIntermediateCheckpoints || processedCount < nextCheckpoint) {
			return nil
		}

		b, err := json.Marshal(state, json.Deterministic(true))
		if err != nil {
			return err
		}
		if !forced {
			checkpointCount++
			nextCheckpoint = processedCount + max(int64(1), (planned+maxIntermediateCheckpoints-1)/maxIntermediateCheckpoints)
		}
		return imp.store.UpdateSyncCheckpointContext(context.WithoutCancel(ctx), syncID, &store.Checkpoint{PageToken: string(b), MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors})
	}
	defer func() {
		if retErr != nil {
			sum.Errors++
			checkpoint, _ := json.Marshal(state, json.Deterministic(true))
			_ = imp.store.FailSyncWithCheckpoint(syncID, retErr.Error(), &store.Checkpoint{PageToken: string(checkpoint), MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors})
		}
	}()
	after := ""
	if !o.CreatedAfter.IsZero() {
		after = o.CreatedAfter.UTC().Format(time.DateOnly)
	}
	if state.WindowEnd == "" || state.Full != o.Full || state.CreatedAfter != after {
		state.Seen = map[string]bool{}
		state.WindowStart = ""
		state.WindowEnd = now.Format(time.DateOnly)
		state.Full = o.Full
		state.CreatedAfter = after
		if !o.Full && state.Watermark != "" {
			t, _ := time.Parse(time.DateOnly, state.Watermark)
			state.WindowStart = t.AddDate(0, 0, -1).Format(time.DateOnly)
		}
	}
	if state.Seen == nil {
		state.Seen = map[string]bool{}
	}
	for id, p := range state.Pending {
		if p.Terminal && !now.Before(p.Until) {
			delete(state.Pending, id)
		}
	}
	if err := save(true); err != nil {
		return sum, err
	}
	processed := map[string]bool{}
	var hardErrors []error
	process := func(id, parent string) (processErr error) {
		if processed[id] {
			return nil
		}
		processed[id] = true
		processedCount++
		p, known := state.Pending[id]
		// Validation failures share the same bounded retry window as absent
		// artifacts. Ordinary/auth/transport failures still fail the run.
		defer func() {
			p.Next = now.Add(6 * time.Hour)
			p.RetryDeferred = processErr != nil
			if processErr != nil && onlyInvalidPayload(processErr) {
				p.Terminal = !now.Before(p.Until)
				if p.Terminal {
					state.Seen[id] = true
					sum.Errors++
					processErr = nil
				}
			}
			if processErr != nil {
				// A failed fetch or archive write must remain retryable even when
				// the artifact window has elapsed. Only successful processing,
				// confirmed absence, or an expired invalid payload is terminal.
				p.Terminal = false
			}
			state.Pending[id] = p
			if p.Terminal {
				delete(state.Pending, id)
			}
		}()
		if !known {
			p = pendingCall{Discovered: now, Until: now.Add(48 * time.Hour), Parent: parent}
			state.Pending[id] = p
		}
		if parent != "" {
			p.Parent = parent
		}
		c, err := imp.client.GetCall(ctx, id)
		if errors.Is(err, ErrNotFound) {
			p.Next = now.Add(6 * time.Hour)
			p.Terminal = !now.Before(p.Until)
			state.Pending[id] = p
			sum.Skipped++
			return nil
		}
		if err != nil {
			p.Next = now.Add(6 * time.Hour)
			state.Pending[id] = p
			return err
		}
		if !c.started().IsZero() && c.duration() != nil && c.ended() {
			p.Until = c.started().Add(time.Duration(*c.duration() * float64(time.Second))).Add(7 * 24 * time.Hour)
		}
		hook, hookErr := imp.client.GetPostCall(ctx, id)
		if errors.Is(hookErr, ErrNotFound) {
			hook = nil
			hookErr = nil
		}
		existing, err := imp.store.MessageExistsBatch(src.ID, []string{id})
		if err != nil {
			return err
		}
		var previous *Evidence
		if messageID := existing[id]; messageID != 0 {
			raw, err := imp.store.GetMessageRaw(messageID)
			if err != nil {
				return err
			}
			var e Evidence
			if err := json.Unmarshal(raw, &e); err != nil || e.Version != 1 {
				return errors.New("invalid archived Bland evidence")
			}
			previous = &e
		}
		metadataHook, usedArchivedMetadata, err := archivedMetadataFallbackHook(previous, hook)
		if err != nil {
			return err
		}
		c, err = enrichedCall(c, metadataHook)
		if err != nil {
			return err
		}
		var correction jsontext.Value
		var correctionErr error
		if o.FetchCorrectedTranscript && c.ended() {
			correction, correctionErr = imp.client.GetCorrectedTranscript(ctx, id)
			if errors.Is(correctionErr, ErrNotFound) {
				correctionErr = nil
				correction = nil
			}
		}
		hookErr = errors.Join(hookErr, correctionErr)
		if !c.started().IsZero() && c.duration() != nil && c.ended() {
			p.Until = c.started().Add(time.Duration(*c.duration() * float64(time.Second))).Add(7 * 24 * time.Hour)
		}
		ev, renditionErr := buildEvidence(c, metadataHook, previous, correction)
		if ev == nil {
			return renditionErr
		}
		if usedArchivedMetadata {
			ev.PostCall = hook
			if len(hook) == 0 && previous != nil {
				ev.PostCall = previous.PostCall
			}
		}
		if p.Parent != "" {
			ev.ParentCallID = p.Parent
		}
		p.Next = now.Add(6 * time.Hour)
		p.Terminal = !now.Before(p.Until)
		state.Pending[id] = p
		// Proxy legs have independent provider IDs and remain separate meetings.
		discoveredProxy := false
		for _, proxy := range ev.ProxyCallIDs {
			if !validID(proxy) || proxy == id {
				continue
			}
			if _, ok := state.Pending[proxy]; !ok {
				state.Pending[proxy] = pendingCall{Discovered: now, Until: now.Add(48 * time.Hour), Parent: id}
				discoveredProxy = true
			}
		}
		if discoveredProxy {
			if err := save(); err != nil {
				return err
			}
		}
		// Active and artifact-free calls still keep a durable retry record.
		hasRecording := c.Record || c.RecordingURL != "" || ev.Recording.State != ""
		hasSummary := ev.Content.Summary.State == meetingcontent.StateAvailable
		if !c.ended() || (ev.Content.Transcript.State != meetingcontent.StateAvailable && !hasRecording && !hasSummary && previous == nil) {
			sum.Skipped++
			return errors.Join(hookErr, renditionErr)
		}
		raw, err := marshalEvidence(ev)
		if err != nil {
			return err
		}
		metadata, err := json.Marshal(struct {
			Provider   string `json:"provider"`
			CallID     string `json:"call_id"`
			From       string `json:"from"`
			To         string `json:"to"`
			Inbound    bool   `json:"inbound"`
			Status     string `json:"status"`
			AnsweredBy string `json:"answered_by"`
			Parent     string `json:"parent_call_id,omitempty"`
		}{SourceType, id, c.From, c.To, c.Inbound, c.Status, c.AnsweredBy, ev.ParentCallID})
		if err != nil {
			return err
		}
		body := "Bland call"
		if ev.Content.Summary.Text != "" {
			body += "\n\nSummary:\n" + ev.Content.Summary.Text
		}
		if ev.Content.Transcript.Text != "" {
			body += "\n\nTranscript:\n" + ev.Content.Transcript.Text
		}
		attendees := []meetingarchive.Person{}
		for _, person := range ev.Content.SourceParticipants {
			attendees = append(attendees, meetingarchive.Person{Phone: person.Phone})
		}
		snapshot := meetingarchive.Snapshot{SourceID: src.ID, AccountEmail: o.AccountEmail, SourceMessageID: id, SourceConversationID: id, Title: "Bland call", StartedAt: c.started(), Body: body, Snippet: snippet(ev.Content), Metadata: metadata, Raw: raw, RawFormat: RawFormat, Attendees: attendees}
		result, archiveErr := meetingarchive.New(imp.store).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: o.Full})
		sum.MeetingsProcessed++
		if result.Created {
			sum.MeetingsAdded++
		} else if result.Changed {
			sum.MeetingsUpdated++
		}
		if archiveErr != nil {
			return archiveErr
		}
		recording, recordErr := imp.record(ctx, result.MessageID, c, ev.Recording, o, p.Terminal)
		if recording != ev.Recording {
			ev.Recording = recording
			snapshot.Raw, err = marshalEvidence(ev)
			if err == nil {
				_, err = meetingarchive.New(imp.store).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{})
			}
			if !result.Changed {
				sum.MeetingsUpdated++
			}
		}
		if o.Progress != nil {
			o.Progress("processed Bland call")
		}

		return errors.Join(hookErr, renditionErr, recordErr, err)
	}
	// Every known call gets a bounded enrichment reconciliation window, including
	// calls whose ordinary transcript was already available on first discovery.
	ids := make([]string, 0, len(state.Pending))
	for id := range state.Pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for index, id := range ids {
		p := state.Pending[id]
		if o.Full || !p.Terminal && !now.Before(p.Next) {
			sum.MaintenanceRetries++
			if err := process(id, p.Parent); err != nil {
				hardErrors = append(hardErrors, err)
			}
		}
		if (index+1)%100 == 0 {
			if err := save(); err != nil {
				return sum, err
			}
		}
	}
	if err := save(); err != nil {
		return sum, err
	}
	if o.Full {
		var afterID int64
		for {
			archived, err := imp.store.ListSourceMessageIDsPageContext(ctx, src.ID, afterID, 100)
			if err != nil {
				return sum, err
			}
			if len(archived) == 0 {
				break
			}
			eligible := make([]store.SourceMessageIdentity, 0, len(archived))
			for _, row := range archived {
				afterID = row.ID
				if processed[row.SourceMessageID] {
					continue
				}
				if !o.CreatedAfter.IsZero() {
					included, err := archivedCallCreatedAfter(ctx, imp.store, row.ID, o.CreatedAfter)
					if err != nil {
						return sum, err
					}
					if !included {
						continue
					}
				}
				eligible = append(eligible, row)
			}
			for _, row := range eligible {
				if _, exists := state.Pending[row.SourceMessageID]; !exists {
					state.Pending[row.SourceMessageID] = pendingCall{Discovered: now, Until: now.Add(48 * time.Hour)}
				}
			}
			if err := save(); err != nil {
				return sum, err
			}
			for _, row := range eligible {
				if processed[row.SourceMessageID] {
					continue
				}
				sum.MaintenanceRetries++
				if err := process(row.SourceMessageID, state.Pending[row.SourceMessageID].Parent); err != nil {
					hardErrors = append(hardErrors, err)
				}
			}
			if err := save(); err != nil {
				return sum, err
			}
		}
	}
	offset := 0
	discovered := 0
	allSeen := map[string]bool{}
	complete := false
	discoveryEstimateApplied := false
	for {
		if err := ctx.Err(); err != nil {
			hardErrors = append(hardErrors, err)
			break
		}
		page, err := imp.client.ListCalls(ctx, ListOptions{Offset: offset, Limit: 100, UpdateStart: state.WindowStart, UpdateEnd: state.WindowEnd, CreatedAfter: after})
		if err != nil {
			hardErrors = append(hardErrors, err)
			break
		}
		if page == nil {
			hardErrors = append(hardErrors, ErrInvalidPayload)
			break
		}
		if len(page.Calls) == 0 {
			complete = true
			break
		}
		if page.Total != nil && !discoveryEstimateApplied {
			discoveryEstimateApplied = true
			planned += int64(*page.Total)
			nextCheckpoint = max(nextCheckpoint, processedCount+max(int64(1), (planned+maxIntermediateCheckpoints-1)/maxIntermediateCheckpoints))
		}
		selected := map[string]bool{}
		remaining := o.Limit - discovered
		for _, listed := range page.Calls {
			id := listed.ID
			if processed[id] || allSeen[id] || state.Seen[id] || selected[id] {
				continue
			}
			if pending, ok := state.Pending[id]; ok && now.Before(pending.Next) && (o.Limit > 0 || pending.RetryDeferred) {
				continue
			}
			if o.Limit > 0 && len(selected) >= remaining {
				break
			}
			selected[id] = true
			if _, exists := state.Pending[id]; !exists {
				state.Pending[id] = pendingCall{Discovered: now, Until: now.Add(48 * time.Hour)}
			}
		}
		if err := save(); err != nil {
			return sum, err
		}
		newIDs := 0
		for _, listed := range page.Calls {
			id := listed.ID
			if allSeen[id] {
				continue
			}
			allSeen[id] = true
			newIDs++
			if processed[id] || state.Seen[id] {
				continue
			}
			if pending, ok := state.Pending[id]; ok && now.Before(pending.Next) && (o.Limit > 0 || pending.RetryDeferred) {
				continue
			}
			if o.Limit > 0 && discovered >= o.Limit {
				sum.PartialCoverage = true
				break
			}
			discovered++
			if err := process(id, state.Pending[id].Parent); err != nil {
				hardErrors = append(hardErrors, err)
			} else {
				state.Seen[id] = true
			}
		}
		if err := save(); err != nil {
			return sum, err
		}
		if sum.PartialCoverage {
			break
		}
		if newIDs == 0 {
			hardErrors = append(hardErrors, errors.New("bland repeated pagination page"))
			break
		}
		offset += len(page.Calls)
		if page.Total != nil && offset >= *page.Total || len(page.Calls) < 100 && page.Total == nil {
			complete = true
			break
		}
		if offset > 10000000 {
			hardErrors = append(hardErrors, errors.New("bland pagination safety limit"))
			break
		}
	}
	// Newly found proxy calls bypass the discovery limit like other maintenance.
	ids = nil
	for id, p := range state.Pending {
		if p.Parent != "" && !processed[id] && !p.Terminal {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := state.Pending[id]
		if !o.Full && !p.Next.IsZero() && now.Before(p.Next) {
			continue
		}
		sum.MaintenanceRetries++
		if err := process(id, state.Pending[id].Parent); err != nil {
			hardErrors = append(hardErrors, err)
		}
	}
	if len(hardErrors) > 0 {
		return sum, errors.Join(errors.Join(hardErrors...), save(true))
	}
	if complete {
		if after == "" {
			state.Watermark = state.WindowEnd
		}
		state.WindowEnd = ""
		state.WindowStart = ""
		state.Seen = nil
	}
	if err := save(true); err != nil {
		return sum, err
	}
	b, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}); err != nil {
		return sum, err
	}
	return sum, imp.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, src.ID, string(b))
}

func archivedMetadataFallbackHook(previous *Evidence, hook jsontext.Value) (jsontext.Value, bool, error) {
	payload := map[string]jsontext.Value{}
	if len(hook) > 0 {
		var err error
		payload, err = postCallPayload(hook)
		if err != nil {
			return nil, false, fmt.Errorf("decode Bland postcall metadata: %w", err)
		}
	}
	retained := retainedCallMetadata(previous)
	usedArchivedMetadata := false
	for _, key := range callMetadataKeys {
		if !rawPresent(payload[key]) && rawPresent(retained[key]) {
			payload[key] = retained[key]
			usedArchivedMetadata = true
		}
	}
	if !usedArchivedMetadata {
		return hook, false, nil
	}
	combined, err := json.Marshal(struct {
		Data struct {
			Payload map[string]jsontext.Value `json:"payload"`
		} `json:"data"`
	}{Data: struct {
		Payload map[string]jsontext.Value `json:"payload"`
	}{Payload: payload}})
	if err != nil {
		return nil, false, fmt.Errorf("marshal archived Bland metadata fallback: %w", err)
	}
	return jsontext.Value(combined), true, nil
}

func archivedCallCreatedAfter(ctx context.Context, st *store.Store, messageID int64, after time.Time) (bool, error) {
	raw, err := st.GetMessageRawContext(ctx, messageID)
	if err != nil {
		return false, fmt.Errorf("read archived Bland call %d for creation-date filter: %w", messageID, err)
	}
	var evidence Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return false, fmt.Errorf("decode archived Bland call %d for creation-date filter: %w", messageID, err)
	}
	callRaw := evidence.EffectiveCall
	if !rawPresent(callRaw) {
		callRaw = evidence.Call
	}
	var call Call
	if !rawPresent(callRaw) {
		return false, nil
	}
	if err := json.Unmarshal(callRaw, &call); err != nil {
		return false, fmt.Errorf("decode archived Bland call %d payload for creation-date filter: %w", messageID, err)
	}
	if call.CreatedAt == "" && rawPresent(evidence.Call) && string(callRaw) != string(evidence.Call) {
		var latest Call
		if err := json.Unmarshal(evidence.Call, &latest); err != nil {
			return false, fmt.Errorf("decode archived Bland call %d latest payload for creation-date filter: %w", messageID, err)
		}
		call.CreatedAt = latest.CreatedAt
	}
	if call.CreatedAt == "" {
		return false, nil
	}
	createdAt, err := time.Parse(time.RFC3339Nano, call.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("parse archived Bland call %d creation date: %w", messageID, err)
	}
	if createdAt.IsZero() {
		return false, nil
	}
	return !createdAt.Before(after), nil
}

func snippet(c meetingcontent.Content) string {
	v := c.Summary.Text
	if v == "" {
		v = c.Transcript.Text
	}
	r := []rune(strings.TrimSpace(v))
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
func proxyCalls(raw []byte) []string {
	var wire struct {
		Warm struct {
			Calls []struct {
				ID string `json:"call_id"`
			} `json:"proxy_agent_calls"`
		} `json:"warm_transfer_call"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return nil
	}
	out := []string{}
	for _, v := range wire.Warm.Calls {
		out = append(out, v.ID)
	}
	return out
}

func proxyCallsFromHook(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var wire struct {
		Data struct {
			Payload jsontext.Value `json:"payload"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return nil
	}
	return proxyCalls(wire.Data.Payload)
}

// onlyInvalidPayload prevents a joined authentication/transport/archive failure
// from being mistaken for a terminally malformed provider rendition.
func onlyInvalidPayload(err error) bool {
	if err == nil {
		return false
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		children := group.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyInvalidPayload(child) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return onlyInvalidPayload(wrapped)
	}
	return errors.Is(err, ErrInvalidPayload)
}
