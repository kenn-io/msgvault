package chatwoot

import (
	"context"
	"errors"
	"fmt"
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
	NoMedia           bool
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
}

func NewImporter(s *store.Store, c *Client) *Importer { return &Importer{store: s, client: c} }

// Register adds only visible selected inboxes. Sync never silently registers a
// newly created inbox; the daemon operator chooses its archive boundary here.
func (imp *Importer) Register(ctx context.Context, selected []int64) ([]*store.Source, error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("chatwoot importer unavailable")
	}
	inboxes, err := imp.client.ListInboxes(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[int64]bool{}
	for _, id := range selected {
		if id <= 0 {
			return nil, errors.New("chatwoot inbox IDs must be positive")
		}
		wanted[id] = true
	}
	visible := map[int64]bool{}
	for _, inbox := range inboxes {
		if inbox.ID <= 0 {
			return nil, errors.New("chatwoot returned invalid inbox ID")
		}
		visible[inbox.ID] = true
	}
	for id := range wanted {
		if !visible[id] {
			return nil, fmt.Errorf("chatwoot inbox %d is not visible to this account token", id)
		}
	}
	var sources []*store.Source
	for _, inbox := range inboxes {
		if len(wanted) > 0 && !wanted[inbox.ID] {
			continue
		}
		if err = ctx.Err(); err != nil {
			return sources, err
		}
		source, sourceErr := imp.store.GetOrCreateSource(SourceType, SourceIdentifier(imp.client.baseURL, imp.client.accountID, inbox.ID))
		if sourceErr != nil {
			return sources, sourceErr
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
	if opts.Full && !state.FullReconciliation {
		state = newSyncState(scope)
		state.FullReconciliation = true
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return nil, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	scoped.agents = map[int64]Actor{}
	scoped.resolvedActors = map[string]int64{}
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
	for _, id := range opts.SelfAgentIDs {
		a := Actor{ID: id, Type: actorUser}
		if enriched, ok := imp.agents[id]; ok {
			a = enriched
		}
		if _, err = imp.resolveActor(ctx, source.ID, a); err != nil {
			return sum, err
		}
		if err = imp.store.AddAccountIdentityContext(ctx, source.ID, imp.actorIdentifier(a), "chatwoot_self_agent"); err != nil {
			return sum, err
		}
	}
	imp.identities, err = imp.store.ListAccountIdentities(source.ID)
	if err != nil {
		return sum, err
	}
	budget := imp.requestBudget
	if budget <= 0 {
		budget = maxSyncRequests
	}
	conversations, pages, nextPage, err := imp.enumerateConversations(ctx, opts.InboxID, max(state.NextPage, 1), budget)
	if err != nil {
		return sum, err
	}
	interval := opts.ReconcileInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	requests := 0
	// Reserve half the message-work budget for saved conversations. This frontier
	// rotates independently of discovery, so new pages cannot starve old history
	// or late media. Four requests allow the context fetch plus a range probe.
	processed := map[int64]bool{}
	savedBudget := budget / 2
	if savedBudget >= 4 {
		keys := make([]string, 0, len(state.Conversations))
		for key, cs := range state.Conversations {
			if len(cs.Pending) > 0 || len(cs.Artifacts) > 0 {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for index, key := range keys {
			if key >= state.NextSavedConversation {
				keys = append(slices.Clone(keys[index:]), keys[:index]...)
				break
			}
		}
		for index, key := range keys {
			if requests >= savedBudget {
				sum.Partial = true
				break
			}
			id, parseErr := strconv.ParseInt(key, 10, 64)
			if parseErr != nil || id <= 0 {
				return sum, errors.New("invalid Chatwoot saved conversation")
			}
			requests++
			c, fetchErr := imp.client.GetConversation(ctx, id)
			if errors.Is(fetchErr, ErrNotFound) {
				// Deleted or no longer visible conversations cannot supply new history.
				// Keep their archived records, and retire the unavailable work item.
				delete(state.Conversations, key)
			} else {
				if fetchErr != nil {
					return sum, fetchErr
				}
				if c.InboxID <= 0 || (c.AccountID != 0 && c.AccountID != imp.client.accountID) {
					return sum, errors.New("chatwoot saved conversation scope mismatch")
				}
				if c.InboxID != opts.InboxID {
					// A moved conversation is discoverable under its new inbox. Retire
					// this source's saved work while keeping its archived records.
					delete(state.Conversations, key)
				} else {
					if _, processErr := imp.processConversation(ctx, source.ID, syncID, c, state, opts, sum, &requests, savedBudget, interval); processErr != nil {
						return sum, processErr
					}
					processed[c.ID] = true
				}
			}
			state.NextSavedConversation = ""
			for offset := 1; offset <= len(keys); offset++ {
				nextKey := keys[(index+offset)%len(keys)]
				nextState := state.Conversations[nextKey]
				if nextState == nil || (len(nextState.Pending) == 0 && len(nextState.Artifacts) == 0) {
					continue
				}
				state.NextSavedConversation = nextKey
				break
			}
			if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
				return sum, err
			}
		}
	}
	for index, c := range conversations {
		if c.ID == state.NextConversation {
			conversations = conversations[index:]
			break
		}
	}
	// Discovery and message work each have a finite budget. Save the page of
	// the first unvisited conversation, so a bounded run cannot skip a prefix.
	sum.Partial = sum.Partial || nextPage > 1
	if len(conversations) == 0 {
		state.NextPage = nextPage
		state.NextConversation = 0
	}
	for index, c := range conversations {
		cs := state.Conversations[strconv.FormatInt(c.ID, 10)]
		if !processed[c.ID] {
			cs, err = imp.processConversation(ctx, source.ID, syncID, c, state, opts, sum, &requests, budget, interval)
			if err != nil {
				return sum, err
			}
		}
		if index+1 < len(conversations) {
			state.NextConversation = conversations[index+1].ID
			state.NextPage = pages[state.NextConversation]
		} else {
			state.NextConversation = 0
			state.NextPage = nextPage
		}
		if len(cs.Pending) > 0 {
			sum.Partial = true
		}
		if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
			return sum, err
		}
		if requests >= budget {
			if index+1 < len(conversations) {
				sum.Partial = true
			}
			break
		}
	}
	// Discovery EOF can be reached before an earlier conversation finishes
	// its bounded history walk. Saved gaps keep the whole sync partial.
	for _, cs := range state.Conversations {
		if len(cs.Pending) > 0 {
			sum.Partial = true
			break
		}
	}
	if state.FullReconciliation && !sum.Partial {
		state.FullReconciliation = false
	}
	blob, err := state.marshal()
	if err != nil {
		return sum, err
	}
	if err = imp.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, blob); err != nil {
		return sum, err
	}
	return sum, nil
}

func (imp *Importer) processConversation(ctx context.Context, sourceID, syncID int64, c Conversation, state *syncState, opts ImportOptions, sum *ImportSummary, requests *int, budget int, interval time.Duration) (*conversationState, error) {
	var err error
	key := strconv.FormatInt(c.ID, 10)
	cs := state.Conversations[key]
	if cs == nil {
		cs = &conversationState{Pending: []idRange{{1, math.MaxInt64}}, Artifacts: map[string]bool{}, WalkingFull: true}
		state.Conversations[key] = cs
	}
	if cs.Artifacts == nil {
		cs.Artifacts = map[string]bool{}
	}
	if len(cs.Pending) == 0 {
		if cs.ReconciledAt.IsZero() || time.Since(cs.ReconciledAt) >= interval {
			cs.Pending = []idRange{{1, math.MaxInt64}}
			cs.WalkingFull = true
		} else {
			cs.Pending = []idRange{{cs.HighWater + 1, math.MaxInt64}}
		}
	}
	artifactIDs := make([]string, 0, len(cs.Artifacts))
	for id := range cs.Artifacts {
		artifactIDs = append(artifactIDs, id)
	}
	beforeWalk := sum.MessagesProcessed
	if err = imp.walkConversation(ctx, sourceID, syncID, c, cs, state, opts, sum, requests, budget); err != nil {
		return cs, err
	}
	// Completed recordings and transcripts can change without message or
	// conversation activity. Their refresh is independent of history cursors.
	slices.Sort(artifactIDs)
	for artifactIndex, id := range artifactIDs {
		if id >= cs.NextArtifact {
			artifactIDs = append(slices.Clone(artifactIDs[artifactIndex:]), artifactIDs[:artifactIndex]...)
			break
		}
	}
	for artifactIndex, idText := range artifactIDs {
		if *requests >= budget || (opts.Limit > 0 && sum.MessagesProcessed-beforeWalk >= opts.Limit) {
			sum.Partial = true
			break
		}
		id, parseErr := strconv.ParseInt(idText, 10, 64)
		if parseErr != nil || id <= 0 || id > math.MaxInt32 {
			return cs, errors.New("invalid Chatwoot artifact checkpoint")
		}
		nextArtifact := artifactIDs[(artifactIndex+1)%len(artifactIDs)]
		*requests++
		messages, fetchErr := imp.client.ListMessages(ctx, c.ID, id, id+1)
		if errors.Is(fetchErr, ErrNotFound) {
			delete(cs.Artifacts, idText)
			cs.NextArtifact = nextArtifact
			continue
		}
		if fetchErr != nil {
			return cs, fetchErr
		}
		if _, err = subtractHandled(idRange{id, id + 1}, messageIDs(messages)); err != nil {
			return cs, err
		}
		if len(messages) == 0 {
			delete(cs.Artifacts, idText)
			cs.NextArtifact = nextArtifact
			continue
		}
		for _, m := range messages {
			if err = imp.validateMessage(c, m, opts); err != nil {
				return cs, err
			}
			if m.Private && !opts.IncludePrivate {
				delete(cs.Artifacts, idText)
				continue
			}
			if err = imp.persistMessage(ctx, sourceID, c, m, opts, sum); err != nil {
				return cs, err
			}
			sum.MessagesProcessed++
		}
		cs.NextArtifact = nextArtifact
	}
	if _, exists := cs.Artifacts[cs.NextArtifact]; !exists {
		cs.NextArtifact = ""
	}
	if len(cs.Pending) > 0 {
		sum.Partial = true
	}
	return cs, nil
}

const maxSyncRequests = 10000

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

func (imp *Importer) enumerateConversations(ctx context.Context, inboxID int64, startPage, budget int) ([]Conversation, map[int64]int, int, error) {
	var conversations []Conversation
	pages := map[int64]int{}
	seen := map[int64]bool{}
	for offset := range budget {
		page := startPage + offset
		if page <= 0 || page == math.MaxInt {
			return nil, nil, 0, errors.New("chatwoot conversation page overflow")
		}
		batch, err := imp.client.ListConversations(ctx, page, inboxID)
		if err != nil {
			return nil, nil, 0, err
		}
		if len(batch) == 0 {
			return conversations, pages, 1, nil
		}
		newCount := 0
		for _, c := range batch {
			if c.ID <= 0 || c.InboxID <= 0 || (c.AccountID != 0 && c.AccountID != imp.client.accountID) {
				return nil, nil, 0, errors.New("chatwoot returned invalid conversation scope")
			}
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			newCount++
			if c.InboxID == inboxID {
				conversations = append(conversations, c)
				pages[c.ID] = page
			}
		}
		if newCount == 0 {
			return nil, nil, 0, errors.New("chatwoot conversation pagination did not advance")
		}
	}
	return conversations, pages, startPage + budget, nil
}

func (imp *Importer) walkConversation(ctx context.Context, sourceID, syncID int64, c Conversation, cs *conversationState, state *syncState, opts ImportOptions, sum *ImportSummary, requests *int, budget int) error {
	used := 0
	probed := false
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
		if !probed {
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
			probed = true
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
		for index, m := range messages {
			if index > 0 && messages[index-1].ID == m.ID {
				continue
			}
			if opts.Limit > 0 && used >= opts.Limit {
				break
			}
			if err = imp.validateMessage(c, m, opts); err != nil {
				return err
			}
			if !m.Private || opts.IncludePrivate {
				if err = imp.persistMessage(ctx, sourceID, c, m, opts, sum); err != nil {
					return err
				}
				if m.ContentType == "voice_call" || len(m.Attachments) > 0 {
					cs.Artifacts[strconv.FormatInt(m.ID, 10)] = true
				}
			}
			used++
			sum.MessagesProcessed++
			handled = append(handled, m.ID)
			cs.HighWater = max(cs.HighWater, m.ID)
		}
		gaps, err := subtractHandled(r, handled)
		if err != nil {
			return err
		}
		cs.Pending = append(gaps, cs.Pending[1:]...)
		if err = imp.checkpoint(ctx, syncID, state, sum); err != nil {
			return err
		}
	}
	if len(cs.Pending) == 0 {
		if cs.WalkingFull {
			cs.ReconciledAt = time.Now().UTC()
			cs.WalkingFull = false
		}
	}
	return nil
}
