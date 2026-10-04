package chatwoot

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type ImportOptions struct {
	InboxID           int64
	SelfAgentIDs      []int64
	IncludePrivate    bool
	Limit             int
	Full              bool
	ReconcileInterval time.Duration
	Media             bool
	MaxMediaBytes     int64
	AttachmentsDir    string
}

type ImportSummary struct {
	SourceID          int64
	Sources           int
	MessagesAdded     int
	MessagesProcessed int
	Meetings          int
	MediaFailures     int
	Partial           bool
}

type Importer struct {
	store          *store.Store
	client         *Client
	agents         map[int64]Actor
	identities     []store.AccountIdentity
	resolvedActors map[string]int64
	requestBudget  int
	boundsProbed   bool
}

// now is replaceable so tests can hold fixtures inside the refresh window.
var now = time.Now

const (
	maxSyncRequests = 10000
	// activityOverlap rereads conversations near the watermark, because
	// Chatwoot orders equal activity times arbitrarily between pages.
	activityOverlap = 10 * time.Minute
	// openBound ends an open message range. Chatwoot message IDs are 32-bit, and
	// releases before 4.17 reject larger bounds.
	openBound = math.MaxInt32
	// artifactWindow bounds how long a recording, transcript or failed
	// download is rechecked. Chatwoot updates them without new activity.
	artifactWindow = 7 * 24 * time.Hour
	// walkQueueLimit is how many conversations a listing queues before they are
	// processed, which keeps the checkpoint small.
	walkQueueLimit  = 100
	selfAgentSignal = "chatwoot_self_agent"
)

func NewImporter(s *store.Store, c *Client) *Importer { return &Importer{store: s, client: c} }

// Register adds the given visible inboxes. Sync never silently registers a
// newly created inbox; the daemon operator chooses its archive boundary here.
func (imp *Importer) Register(ctx context.Context, inboxes []Inbox) ([]*store.Source, error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("chatwoot importer unavailable")
	}
	var sources []*store.Source
	for _, inbox := range inboxes {
		if inbox.ID <= 0 {
			return sources, errors.New("chatwoot returned invalid inbox ID")
		}
		if err := ctx.Err(); err != nil {
			return sources, err
		}
		source, err := imp.store.GetOrCreateSource(SourceType, SourceIdentifier(imp.client.baseURL, imp.client.accountID, inbox.ID))
		if err != nil {
			return sources, err
		}
		if err = imp.store.UpdateSourceDisplayNameContext(ctx, source.ID, inbox.Name); err != nil {
			return sources, err
		}
		source.DisplayName.String = inbox.Name
		source.DisplayName.Valid = inbox.Name != ""
		sources = append(sources, source)
	}
	if len(sources) == 0 {
		return nil, errors.New("no selected Chatwoot inboxes are visible")
	}
	return sources, nil
}

func (imp *Importer) resumeState(sourceID int64, scope string) (*syncState, error) {
	state := newSyncState(scope)
	prior, err := imp.store.GetLastSuccessfulSyncByType(sourceID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	if prior != nil && prior.CursorAfter.Valid {
		state, err = parseSyncState(prior.CursorAfter.String, scope)
		if err != nil {
			return nil, err
		}
	}
	checkpoint, err := imp.store.GetLatestCheckpointedSyncByType(sourceID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	// Each checkpoint is a complete snapshot, including all older saved gaps.
	if checkpoint != nil && checkpoint.CursorBefore.Valid && (prior == nil || checkpoint.ID > prior.ID) {
		state, err = parseSyncState(checkpoint.CursorBefore.String, scope)
		if err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (imp *Importer) checkpoint(ctx context.Context, syncID int64, state *syncState, sum *ImportSummary) error {
	blob, err := state.marshal()
	if err != nil {
		return err
	}
	return imp.store.UpdateSyncCheckpointContext(ctx, syncID, &store.Checkpoint{PageToken: blob, MessagesAdded: int64(sum.MessagesAdded), MessagesProcessed: int64(sum.MessagesProcessed), ErrorsCount: int64(sum.MediaFailures)})
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, resultErr error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("chatwoot importer unavailable")
	}
	if opts.InboxID <= 0 || opts.Limit < 0 || opts.MaxMediaBytes < 0 {
		return nil, errors.New("invalid Chatwoot import options")
	}
	for _, id := range opts.SelfAgentIDs {
		if id <= 0 {
			return nil, errors.New("chatwoot self agent IDs must be positive")
		}
	}
	scope := SourceIdentifier(imp.client.baseURL, imp.client.accountID, opts.InboxID)
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, scope)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("chatwoot inbox is not registered; run add-chatwoot first")
	}
	state, err := imp.resumeState(source.ID, scope)
	if err != nil {
		return nil, err
	}
	interval := opts.ReconcileInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	switch {
	case opts.Full && state.Walk != walkFull:
		state.Walk, state.NextPage, state.WalkStartedAt = walkFull, 1, now().UTC()
	case state.Walk == "" && now().Sub(state.ReconciledAt) >= interval:
		state.Walk, state.NextPage, state.WalkStartedAt = walkReconcile, 1, now().UTC()
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return nil, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	scoped.agents = map[int64]Actor{}
	scoped.resolvedActors = map[string]int64{}
	scoped.boundsProbed = false
	imp = &scoped
	sum = &ImportSummary{SourceID: source.ID, Sources: 1}
	defer func() {
		if resultErr != nil {
			blob, _ := state.marshal()
			_ = imp.store.FailSyncWithCheckpoint(syncID, "Chatwoot sync did not complete", &store.Checkpoint{PageToken: blob, MessagesAdded: int64(sum.MessagesAdded), MessagesProcessed: int64(sum.MessagesProcessed), ErrorsCount: int64(sum.MediaFailures) + 1})
		}
	}()
	// Preserve resumed progress before any network request can fail.
	if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
		return sum, err
	}
	agents, agentErr := imp.client.ListAgents(ctx)
	if ctx.Err() != nil {
		return sum, ctx.Err()
	}
	if agentErr == nil {
		for _, a := range agents {
			a.Type = actorUser
			imp.agents[a.ID] = a
		}
	}
	if err = imp.syncSelfAgents(ctx, source.ID, opts.SelfAgentIDs); err != nil {
		return sum, err
	}
	imp.identities, err = imp.store.ListAccountIdentities(source.ID)
	if err != nil {
		return sum, err
	}
	budget := imp.requestBudget
	if budget <= 0 {
		budget = maxSyncRequests
	}
	requests := 0
	listed := map[int64]Conversation{}
	reached, err := imp.scanActivity(ctx, source.ID, opts.InboxID, state, listed, &requests, budget/2)
	if err != nil {
		return sum, err
	}
	// A listing queues a bounded batch, which is processed before the next
	// batch is listed, so one run backfills as far as its budget allows.
	visited := map[string]bool{}
	for {
		before := requests
		if err = imp.listNextPages(ctx, source.ID, opts.InboxID, state, listed, &requests, budget); err != nil {
			return sum, err
		}
		if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
			return sum, err
		}
		if err = imp.processSavedWork(ctx, source.ID, syncID, state, listed, visited, opts, sum, &requests, budget); err != nil {
			return sum, err
		}
		if state.Walk == "" || state.NextPage == 0 || requests >= budget || requests == before {
			break
		}
	}
	if state.Walk != "" && state.NextPage > 0 {
		sum.Partial = true
	}
	for _, cs := range state.Conversations {
		if len(cs.Pending) > 0 {
			sum.Partial = true
			break
		}
	}
	if state.Walk != "" && state.NextPage == 0 && !sum.Partial {
		// The next reconcile rereads what was written since this one started.
		state.Walk, state.ReconciledAt = "", state.WalkStartedAt
	}
	// A scan that ran out of budget leaves changed conversations for the next
	// sync, but the listing's own completion doesn't depend on it.
	sum.Partial = sum.Partial || !reached
	blob, err := state.marshal()
	if err != nil {
		return sum, err
	}
	if err = imp.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, blob); err != nil {
		return sum, err
	}
	return sum, nil
}

// syncSelfAgents keeps the importer's own identities equal to self_agent_ids.
// Store derives ownership from identities, so removal un-marks earlier records.
func (imp *Importer) syncSelfAgents(ctx context.Context, sourceID int64, selfAgentIDs []int64) error {
	wanted := map[string]bool{}
	for _, id := range selfAgentIDs {
		a := Actor{ID: id, Type: actorUser}
		if enriched, ok := imp.agents[id]; ok {
			a = enriched
		}
		if _, err := imp.resolveActor(ctx, sourceID, a); err != nil {
			return err
		}
		address := imp.actorIdentifier(a)
		wanted[address] = true
		if err := imp.store.AddAccountIdentityContext(ctx, sourceID, address, selfAgentSignal); err != nil {
			return err
		}
	}
	identities, err := imp.store.ListAccountIdentities(sourceID)
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if identity.SourceSignal == selfAgentSignal && !wanted[identity.Address] {
			if _, err = imp.store.RemoveAccountIdentityContext(ctx, sourceID, identity.Address); err != nil {
				return err
			}
		}
	}
	return nil
}

func (imp *Importer) listConversations(ctx context.Context, page int, inboxID int64, sortBy string) ([]Conversation, error) {
	if page <= 0 || page == math.MaxInt {
		return nil, errors.New("chatwoot conversation page overflow")
	}
	batch, err := imp.client.ListConversations(ctx, page, inboxID, sortBy)
	if err != nil {
		return nil, err
	}
	for index, c := range batch {
		if c.ID <= 0 || c.InboxID <= 0 || (c.AccountID != 0 && c.AccountID != imp.client.accountID) || c.LastActivityAt <= 0 {
			return nil, errors.New("chatwoot returned invalid conversation scope")
		}
		if sortBy == sortByActivity && index > 0 && c.LastActivityAt > batch[index-1].LastActivityAt {
			return nil, errors.New("chatwoot did not honor conversation activity order")
		}
	}
	return batch, nil
}

// scanActivity queues conversations whose activity is newer than the archive.
// It runs every sync, stops at the saved watermark, and reports whether it
// reached it within budget.
func (imp *Importer) scanActivity(ctx context.Context, sourceID, inboxID int64, state *syncState, listed map[int64]Conversation, requests *int, budget int) (bool, error) {
	cutoff := state.ActivityWatermark - int64(activityOverlap/time.Second)
	newest := state.ActivityWatermark
	for page, done := 1, false; !done; page++ {
		if *requests >= budget {
			return false, nil
		}
		*requests++
		batch, err := imp.listConversations(ctx, page, inboxID, sortByActivity)
		if err != nil {
			return false, err
		}
		// The first sync only records the watermark; its listing covers the rest.
		done = len(batch) == 0 || state.ActivityWatermark == 0
		for _, c := range batch {
			newest = max(newest, c.LastActivityAt)
			if c.LastActivityAt < cutoff {
				done = true
			} else if state.ActivityWatermark > 0 && c.InboxID == inboxID {
				if err = imp.enqueue(ctx, sourceID, state, c, "", listed); err != nil {
					return false, err
				}
			}
		}
	}
	state.ActivityWatermark = newest
	return true, nil
}

// listNextPages continues the listing of every conversation, which runs only
// for --full and the periodic reconcile, until a batch is queued.
func (imp *Importer) listNextPages(ctx context.Context, sourceID, inboxID int64, state *syncState, listed map[int64]Conversation, requests *int, budget int) error {
	for state.Walk != "" && state.NextPage > 0 {
		queued := 0
		for _, cs := range state.Conversations {
			if len(cs.Pending) > 0 {
				queued++
			}
		}
		if *requests >= budget || queued >= walkQueueLimit {
			return nil
		}
		*requests++
		batch, err := imp.listConversations(ctx, state.NextPage, inboxID, sortByCreated)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			// The walk ends when the queued conversations finish, so a bounded
			// --full run resumes its saved ranges instead of restarting.
			state.NextPage = 0
			break
		}
		for _, c := range batch {
			if c.InboxID == inboxID {
				if err = imp.enqueue(ctx, sourceID, state, c, state.Walk, listed); err != nil {
					return err
				}
			}
		}
		state.NextPage++
	}
	return nil
}

// enqueue saves history work for one listed conversation. A full walk rereads
// all of it. Otherwise only messages above the archived ones are read, and
// nothing when the archive already holds the conversation's newest message.
func (imp *Importer) enqueue(ctx context.Context, sourceID int64, state *syncState, c Conversation, walk string, listed map[int64]Conversation) error {
	key := strconv.FormatInt(c.ID, 10)
	listed[c.ID] = c
	cs := state.Conversations[key]
	if cs == nil {
		cs = &conversationState{}
	}
	// Messages written concurrently can commit out of ID order, so a read can
	// miss one on either side of the archived head. Reconcile rereads the whole
	// history of each conversation active since the last one.
	switch {
	case walk == walkFull || (walk == walkReconcile && c.LastActivityAt >= state.ReconciledAt.Add(-activityOverlap).Unix()):
		cs.Pending = []idRange{{1, openBound}}
	case len(cs.Pending) > 0:
	default:
		head, err := imp.store.ChatwootConversationHead(ctx, sourceID, key)
		if err != nil {
			return err
		}
		// The listing names the newest message, so an archive holding it is
		// current. A newest private note excluded by policy is reread when listed.
		if c.LastMessageID <= head {
			return nil
		}
		cs.Pending = []idRange{{head + 1, openBound}}
	}
	state.Conversations[key] = cs
	return nil
}

// processSavedWork rotates through conversations with pending history or live
// artifacts, so a bounded run cannot starve any of them.
func (imp *Importer) processSavedWork(ctx context.Context, sourceID, syncID int64, state *syncState, listed map[int64]Conversation, visited map[string]bool, opts ImportOptions, sum *ImportSummary, requests *int, budget int) error {
	expired := now().Add(-artifactWindow).Unix()
	keys := make([]string, 0, len(state.Conversations))
	for key, cs := range state.Conversations {
		for id, createdAt := range cs.Artifacts {
			if createdAt < expired {
				delete(cs.Artifacts, id)
			}
		}
		if cs.idle() {
			delete(state.Conversations, key)
			continue
		}
		// Each conversation gets one turn per run, so --limit stays per run.
		if !visited[key] {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for index, key := range keys {
		if key > state.LastSavedConversation {
			keys = append(slices.Clone(keys[index:]), keys[:index]...)
			break
		}
	}
	for _, key := range keys {
		if *requests >= budget {
			sum.Partial = true
			break
		}
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 {
			return errors.New("invalid Chatwoot saved conversation")
		}
		cs := state.Conversations[key]
		c, ok := listed[id]
		if !ok {
			*requests++
			c, err = imp.client.GetConversation(ctx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				// Deleted or no longer visible conversations cannot supply new history.
				// Keep their archived records, and retire the unavailable work item.
				cs = &conversationState{}
			case err != nil:
				return err
			case c.InboxID <= 0 || (c.AccountID != 0 && c.AccountID != imp.client.accountID):
				return errors.New("chatwoot saved conversation scope mismatch")
			case c.InboxID != opts.InboxID:
				// A moved conversation is discoverable under its new inbox. Retire
				// this source's saved work while keeping its archived records.
				cs = &conversationState{}
			}
		}
		if !cs.idle() {
			if err = imp.processConversation(ctx, sourceID, syncID, c, cs, state, opts, sum, requests, budget); err != nil {
				return err
			}
		}
		if cs.idle() {
			delete(state.Conversations, key)
		}
		state.LastSavedConversation = key
		visited[key] = true
		if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
			return err
		}
	}
	return nil
}

func (imp *Importer) processConversation(ctx context.Context, sourceID, syncID int64, c Conversation, cs *conversationState, state *syncState, opts ImportOptions, sum *ImportSummary, requests *int, budget int) error {
	if cs.Artifacts == nil {
		cs.Artifacts = map[string]int64{}
	}
	artifactIDs := make([]int64, 0, len(cs.Artifacts))
	for idText := range cs.Artifacts {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 || id >= openBound {
			return errors.New("invalid Chatwoot artifact checkpoint")
		}
		artifactIDs = append(artifactIDs, id)
	}
	slices.Sort(artifactIDs)
	if err := imp.walkConversation(ctx, sourceID, syncID, c, cs, state, opts, sum, requests, budget); err != nil {
		return err
	}
	if len(cs.Pending) > 0 {
		sum.Partial = true
	}
	// Recordings and transcripts change without conversation activity. One range
	// read covers a conversation's artifacts. A capped read resumes past its last
	// ID; one it skipped below that is read on its own, since creation order can
	// put it past the cap on every read.
	for len(artifactIDs) > 0 {
		if *requests >= budget {
			sum.Partial = true
			return nil
		}
		r := idRange{artifactIDs[0], artifactIDs[len(artifactIDs)-1] + 1}
		single := len(artifactIDs) == 1
		*requests++
		messages, err := imp.client.ListMessages(ctx, c.ID, r.After, r.Before)
		if errors.Is(err, ErrNotFound) {
			clear(cs.Artifacts)
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = subtractHandled(r, messageIDs(messages)); err != nil {
			return err
		}
		returned := make(map[int64]Message, len(messages))
		var last int64
		for _, m := range messages {
			returned[m.ID] = m
			last = max(last, m.ID)
		}
		capped := len(messages) >= imp.client.messageRangeCap && !single
		var below, above []int64
		for _, id := range artifactIDs {
			idText := strconv.FormatInt(id, 10)
			m, ok := returned[id]
			switch {
			case !ok && capped && id > last:
				above = append(above, id)
			case !ok && capped:
				below = append(below, id)
			case !ok:
				delete(cs.Artifacts, idText)
			case m.Private && !opts.IncludePrivate:
				delete(cs.Artifacts, idText)
			default:
				if err = imp.validateMessage(c, m, opts); err != nil {
					return err
				}
				refreshFrom, persistErr := imp.persistMessage(ctx, sourceID, c, m, opts, sum)
				if persistErr != nil {
					return persistErr
				}
				// The window counts from first sighting, so rechecks never extend it.
				if refreshFrom == 0 {
					delete(cs.Artifacts, idText)
				}
				sum.MessagesProcessed++
			}
		}
		for _, id := range below {
			if *requests >= budget {
				sum.Partial = true
				return nil
			}
			if err = imp.refreshArtifact(ctx, sourceID, c, cs, id, opts, sum, requests); err != nil {
				return err
			}
		}
		artifactIDs = above
	}
	return nil
}

// refreshArtifact rereads one pending artifact by its exact ID.
func (imp *Importer) refreshArtifact(ctx context.Context, sourceID int64, c Conversation, cs *conversationState, id int64, opts ImportOptions, sum *ImportSummary, requests *int) error {
	idText := strconv.FormatInt(id, 10)
	*requests++
	messages, err := imp.client.ListMessages(ctx, c.ID, id, id+1)
	if errors.Is(err, ErrNotFound) {
		delete(cs.Artifacts, idText)
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = subtractHandled(idRange{id, id + 1}, messageIDs(messages)); err != nil {
		return err
	}
	if len(messages) == 0 || (messages[0].Private && !opts.IncludePrivate) {
		delete(cs.Artifacts, idText)
		return nil
	}
	if err = imp.validateMessage(c, messages[0], opts); err != nil {
		return err
	}
	refreshFrom, err := imp.persistMessage(ctx, sourceID, c, messages[0], opts, sum)
	if err != nil {
		return err
	}
	if refreshFrom == 0 {
		delete(cs.Artifacts, idText)
	}
	sum.MessagesProcessed++
	return nil
}

func messageIDs(messages []Message) []int64 {
	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func (imp *Importer) validateMessage(c Conversation, m Message, opts ImportOptions) error {
	if m.ID <= 0 || m.ID > math.MaxInt32 || (m.ConversationID != 0 && m.ConversationID != c.ID) || (m.InboxID != 0 && m.InboxID != opts.InboxID) || (m.AccountID != 0 && m.AccountID != imp.client.accountID) {
		return errors.New("chatwoot message scope does not match the selected inbox conversation")
	}
	for _, a := range m.Attachments {
		if a.ID < 0 || (a.MessageID != 0 && a.MessageID != m.ID) {
			return errors.New("chatwoot attachment scope mismatch")
		}
	}
	return nil
}

func (imp *Importer) walkConversation(ctx context.Context, sourceID, syncID int64, c Conversation, cs *conversationState, state *syncState, opts ImportOptions, sum *ImportSummary, requests *int, budget int) error {
	used := 0
	for len(cs.Pending) > 0 && *requests < budget && (opts.Limit == 0 || used < opts.Limit) {
		r := cs.Pending[0]
		*requests++
		messages, err := imp.client.ListMessages(ctx, c.ID, r.After, r.Before)
		if err != nil {
			return err
		}
		if _, err = subtractHandled(r, messageIDs(messages)); err != nil {
			return err
		}
		if len(messages) == 0 {
			cs.Pending = cs.Pending[1:]
			if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
				return err
			}
			continue
		}
		if !imp.boundsProbed {
			id := messages[0].ID
			exact, probeErr := imp.client.ListMessages(ctx, c.ID, id, id+1)
			*requests++
			if probeErr != nil {
				return probeErr
			}
			if len(exact) != 1 || exact[0].ID != id {
				return errors.New("chatwoot API does not support exact combined message bounds")
			}
			empty, probeErr := imp.client.ListMessages(ctx, c.ID, id, id)
			*requests++
			if probeErr != nil {
				return probeErr
			}
			if len(empty) != 0 {
				return errors.New("chatwoot API does not support empty combined message bounds")
			}
			imp.boundsProbed = true
		}
		slices.SortFunc(messages, func(a, b Message) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		var handled []int64
		cut := false
		for index, m := range messages {
			if index > 0 && messages[index-1].ID == m.ID {
				continue
			}
			if opts.Limit > 0 && used >= opts.Limit {
				cut = true
				break
			}
			if err = imp.validateMessage(c, m, opts); err != nil {
				return err
			}
			if !m.Private || opts.IncludePrivate {
				refreshFrom, persistErr := imp.persistMessage(ctx, sourceID, c, m, opts, sum)
				if persistErr != nil {
					return persistErr
				}
				if refreshFrom > 0 && now().Sub(time.Unix(refreshFrom, 0)) < artifactWindow {
					cs.Artifacts[strconv.FormatInt(m.ID, 10)] = refreshFrom
				}
			}
			used++
			sum.MessagesProcessed++
			handled = append(handled, m.ID)
		}
		gaps, err := subtractHandled(r, handled)
		if err != nil {
			return err
		}
		// Chatwoot caps a bounded range response, so a shorter one holds the
		// whole range once every message in it is handled.
		switch {
		case cut && len(handled) > 0 && len(messages) < imp.client.messageRangeCap:
			// The whole range came back, so only the unhandled tail is unproven.
			gaps = []idRange{{handled[len(handled)-1] + 1, r.Before}}
		case cut:
		case len(messages) < imp.client.messageRangeCap:
			gaps = nil
		case len(handled) >= 2:
			// A capped response proves nothing about its holes, since a later-created
			// message can have a lower ID. Splitting at the middle ID rereads each
			// half, so cost grows with pages, not holes.
			middle := handled[len(handled)/2]
			gaps = []idRange{{r.After, middle}, {middle, r.Before}}
		}
		cs.Pending = append(gaps, cs.Pending[1:]...)
		if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
			return err
		}
	}
	return nil
}
