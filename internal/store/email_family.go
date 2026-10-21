package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	emailFamilyMemberLimit = 500
	emailFamilyKeyLimit    = 5000
)

// Evidence names how a related copy connects to the opened email.
const (
	EmailFamilyEvidenceAnchor            = "anchor"
	EmailFamilyEvidenceCopy              = "copy"
	EmailFamilyEvidenceReply             = "reply"
	EmailFamilyEvidenceRepliedTo         = "replied_to"
	EmailFamilyEvidenceForward           = "forward"
	EmailFamilyEvidenceForwardedOriginal = "forwarded_original"
	EmailFamilyEvidenceReferences        = "references"
	EmailFamilyEvidenceIndirect          = "indirect"
	EmailFamilyEvidenceThread            = "thread"
)

// The limits a truncated family can hit.
const (
	EmailFamilyLimitMembers     = "members"
	EmailFamilyLimitIdentifiers = "identifiers"
)

// EmailFamilyMember is one archived copy, kept distinct from the others.
type EmailFamilyMember struct {
	MessageID        int64      `json:"message_id"`
	SourceID         int64      `json:"source_id"`
	SourceType       string     `json:"source_type"`
	SourceIdentifier string     `json:"source_identifier"`
	AccountAddress   string     `json:"account_address,omitempty"`
	AccountPath      string     `json:"account_path,omitempty"`
	Draft            bool       `json:"draft"`
	ConversationID   int64      `json:"conversation_id"`
	Subject          string     `json:"subject,omitempty"`
	SentAt           *time.Time `json:"sent_at,omitempty"`
	// LinkageOutcome is the copy's email_linkage_outcome, "pending" before its
	// identifiers are derived. Anything but "complete" means the copy may have
	// relations this response cannot show.
	LinkageOutcome string `json:"linkage_outcome"`
	// Evidence is the relation to the opened email.
	Evidence string `json:"evidence"`
}

// EmailFamily is the opened email, its other archived copies, and the emails
// connected to them by explicit Message-ID, In-Reply-To, References or
// attached-original headers, plus the rest of the provider threads of the email and its exact copies.
type EmailFamily struct {
	AnchorMessageID int64               `json:"anchor_message_id"`
	Members         []EmailFamilyMember `json:"members"`
	Truncated       bool                `json:"truncated"`
	// TruncatedReason names the limit that was hit, "members" or "identifiers".
	// Reaching the identifier limit can truncate a family of fewer than 500 emails.
	TruncatedReason string `json:"truncated_reason,omitempty"`
	// PendingLinkage is true when any email in the caller's sources has no
	// derived identifiers yet. Such an email can be a related copy this walk
	// cannot see, so an empty or short family is not complete.
	PendingLinkage bool `json:"pending_linkage"`
}

type emailFamilyNode struct {
	EmailFamilyMember

	ownID string
	keys  map[string][]string
}

// allKeys lists every identifier the copy names.
func (n *emailFamilyNode) allKeys() []string {
	return slices.Concat(n.keys["reply"], n.keys["reference"], n.keys["attached"])
}

// identifiers lists what a lookup matches for the copy: its own Message-ID
// and every identifier it names.
func (n *emailFamilyNode) identifiers() []string {
	return append([]string{n.ownID}, n.allKeys()...)
}

// EmailFamilyContext walks outward from one email through shared identifiers
// and the provider threads of the email and its exact copies in a single repeatable snapshot, under the
// caller's source grant. Reads never derive missing data or fetch provider
// content.
//
// Every step reads at most the room left under the 500-copy limit, plus one to
// show the limit was hit. When a step finds more than fits, the copies that
// relate to the opened email most directly stay: the same Message-ID, then a
// direct reply or forward, then shared references, then anything reached
// through another copy or only by conversation.
func (s *Store) EmailFamilyContext(ctx context.Context, id int64) (*EmailFamily, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")

	anchors, err := s.emailFamilyNodes(ctx, []int64{id}, scope, scopeArgs)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("email family of message %d: %w", id, ErrMessageNotFound)
	}
	anchor := anchors[0]
	anchor.Evidence = EmailFamilyEvidenceAnchor
	nodes := map[int64]*emailFamilyNode{id: anchor}
	family := &EmailFamily{AnchorMessageID: id}
	truncate := func(reason string) {
		family.Truncated = true
		if family.TruncatedReason == "" || reason == EmailFamilyLimitMembers {
			family.TruncatedReason = reason
		}
	}
	walk := &emailFamilyWalk{store: s, scope: scope, scopeArgs: scopeArgs, nodes: nodes}
	looked := map[string]bool{}
	frontier := []*emailFamilyNode{anchor}
	for first := true; len(frontier) > 0; first = false {
		var identifiers []string
		for _, node := range frontier {
			for _, identifier := range node.identifiers() {
				if identifier == "" || looked[identifier] {
					continue
				}
				if len(looked) == emailFamilyKeyLimit {
					truncate(EmailFamilyLimitIdentifiers)
					continue
				}
				looked[identifier] = true
				identifiers = append(identifiers, identifier)
			}
		}
		limit := emailFamilyMemberLimit - len(nodes) + 1
		var found []int64
		if first {
			found, err = walk.firstStep(ctx, anchor, identifiers, limit)
		} else {
			found, err = walk.step(ctx, identifiers, limit)
		}
		if err != nil {
			return nil, err
		}
		loaded, err := s.emailFamilyNodes(ctx, found, scope, scopeArgs)
		if err != nil {
			return nil, err
		}
		for _, node := range loaded {
			node.Evidence = emailFamilyEvidence(anchor, node, walk.threads)
		}
		slices.SortFunc(loaded, func(a, b *emailFamilyNode) int {
			if rank := emailFamilyEvidenceRank(a.Evidence) - emailFamilyEvidenceRank(b.Evidence); rank != 0 {
				return rank
			}
			return cmp.Compare(a.MessageID, b.MessageID)
		})
		if room := emailFamilyMemberLimit - len(nodes); len(loaded) > room {
			loaded = loaded[:room]
			truncate(EmailFamilyLimitMembers)
		}
		for _, node := range loaded {
			nodes[node.MessageID] = node
		}
		frontier = loaded
	}
	// One probe of idx_messages_email_linkage_pending, in the same snapshot.
	pendingScope, pendingArgs := sourceScopeClause(ctx, "source_id")
	err = s.db.QueryRowContext(ctx, emailFamilyPendingQuery(pendingScope), pendingArgs...).Scan(&family.PendingLinkage)
	if err != nil {
		return nil, fmt.Errorf("check pending email linkage: %w", err)
	}
	for _, node := range nodes {
		family.Members = append(family.Members, node.EmailFamilyMember)
	}
	slices.SortFunc(family.Members, func(a, b EmailFamilyMember) int { return cmp.Compare(a.MessageID, b.MessageID) })
	return family, nil
}

// emailFamilyEvidenceRank orders evidence by how directly it ties a copy to the
// opened email; lower stays when the member limit cuts.
func emailFamilyEvidenceRank(evidence string) int {
	switch evidence {
	case EmailFamilyEvidenceCopy:
		return 0
	case EmailFamilyEvidenceReply, EmailFamilyEvidenceRepliedTo, EmailFamilyEvidenceForward, EmailFamilyEvidenceForwardedOriginal:
		return 1
	case EmailFamilyEvidenceReferences:
		return 2
	case EmailFamilyEvidenceThread:
		return 4
	}
	return 3
}

// emailFamilyLiveClause keeps the lookups to live emails.
const emailFamilyLiveClause = `m.deleted_at IS NULL AND COALESCE(m.message_type, '') IN ('', 'email')`

// emailFamilyDirectKinds limits an identifier lookup to the links that name an
// email directly, as a reply or as a forwarded original.
const emailFamilyDirectKinds = ` AND k.kind IN ('reply', 'attached')`

// emailFamilyLookupQueries returns the two identifier lookups, each with one
// %s for the placeholders and scope arguments ahead of the identifiers. The
// first finds emails whose own Message-ID is one of the identifiers, through
// the canonical rfc822_message_id index. The second reads idx_email_link_keys_value
// for emails that name one of them, and checks each one against the live,
// in-scope messages with EXISTS. A message that names several identifiers
// appears more than once, which the caller removes.
func (s *Store) emailFamilyLookupQueries(scope, kinds string) (byOwnID, byKey string) {
	byOwnID = `SELECT m.id FROM messages m WHERE ` + emailFamilyLiveClause + scope +
		` AND ` + s.dialect.RFC822CanonicalIDExpr("m.rfc822_message_id") + ` IN (%s)`
	byKey = `SELECT k.message_id FROM email_link_keys k
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id = k.message_id AND ` + emailFamilyLiveClause + scope + `)
		AND k.value IN (%s)` + kinds
	return byOwnID, byKey
}

// errEmailFamilyEnough stops a lookup that has found all it may keep.
var errEmailFamilyEnough = errors.New("email family lookup limit reached")

type emailFamilyWalk struct {
	store     *Store
	scope     string
	scopeArgs []any
	// nodes are the copies already kept; lookups skip them, so they never
	// use up the limit meant for new ones.
	nodes map[int64]*emailFamilyNode
	// threads are the provider threads of the opened email and its exact
	// copies, so members of them may be labeled thread.
	threads map[int64]bool
}

// collect runs one lookup and returns at most limit distinct IDs. It stops
// reading as soon as it has them, so a popular identifier costs limit rows
// instead of every row that names it.
func (w *emailFamilyWalk) collect(ctx context.Context, query string, binds []any, limit int, into *emailFamilyIDs) error {
	err := queryInChunksContext(ctx, w.store.db, binds, w.scopeArgs, query, func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if w.nodes[id] == nil {
			into.add(id)
		}
		if len(into.ids) >= limit {
			return errEmailFamilyEnough
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEmailFamilyEnough) {
		return err
	}
	return nil
}

// emailFamilyIDs keeps IDs in the order first found, without repeats.
type emailFamilyIDs struct {
	ids  []int64
	seen map[int64]bool
}

func (s *emailFamilyIDs) add(id int64) {
	if s.seen == nil {
		s.seen = map[int64]bool{}
	}
	if !s.seen[id] {
		s.seen[id] = true
		s.ids = append(s.ids, id)
	}
}

func (w *emailFamilyWalk) lookups(ctx context.Context, identifiers []string, kinds string, limit int, into *emailFamilyIDs) error {
	if len(identifiers) == 0 {
		return nil
	}
	byOwnID, byKey := w.store.emailFamilyLookupQueries(w.scope, kinds)
	if kinds == "" {
		if err := w.collect(ctx, byOwnID, w.store.emailFamilyOwnIDBinds(identifiers), limit, into); err != nil {
			return fmt.Errorf("find email family by message id: %w", err)
		}
	}
	keys := make([]any, len(identifiers))
	for i, identifier := range identifiers {
		keys[i] = identifier
	}
	if err := w.collect(ctx, byKey, keys, limit, into); err != nil {
		return fmt.Errorf("find email family by linkage key: %w", err)
	}
	return nil
}

// step returns the IDs of live emails in scope that own or name any of the
// identifiers, at most limit of them.
func (w *emailFamilyWalk) step(ctx context.Context, identifiers []string, limit int) ([]int64, error) {
	var found emailFamilyIDs
	err := w.lookups(ctx, identifiers, "", limit, &found)
	return found.ids, err
}

// firstStep finds what ties the opened email to others. It runs the most direct
// lookups first, each with its own limit, so a flood of weaker evidence cannot
// crowd them out: copies of the same Message-ID, then emails that reply to it
// or attach it, then everything its identifiers reach, then the rest of its
// provider threads of the email and its exact copies. Nothing beyond this step
// adds a thread.
func (w *emailFamilyWalk) firstStep(ctx context.Context, anchor *emailFamilyNode, identifiers []string, limit int) ([]int64, error) {
	var found emailFamilyIDs
	var copies []int64
	if anchor.ownID != "" {
		byOwnID, byKey := w.store.emailFamilyLookupQueries(w.scope, emailFamilyDirectKinds)
		if err := w.collect(ctx, byOwnID, w.store.emailFamilyOwnIDBinds([]string{anchor.ownID}), limit, &found); err != nil {
			return nil, fmt.Errorf("find email family copies: %w", err)
		}
		copies = slices.Clone(found.ids)
		if err := w.collect(ctx, byKey, []any{anchor.ownID}, limit, &found); err != nil {
			return nil, fmt.Errorf("find email family replies: %w", err)
		}
	}
	// The emails it replies to or attaches.
	if direct := slices.Concat(anchor.keys["reply"], anchor.keys["attached"]); len(direct) > 0 {
		byOwnID, _ := w.store.emailFamilyLookupQueries(w.scope, "")
		if err := w.collect(ctx, byOwnID, w.store.emailFamilyOwnIDBinds(direct), limit, &found); err != nil {
			return nil, fmt.Errorf("find email family parents: %w", err)
		}
	}
	if err := w.lookups(ctx, identifiers, "", limit, &found); err != nil {
		return nil, err
	}
	// Only a provider's own thread is evidence. Importers and IMAP derive the
	// conversation from References, which can join unrelated mail.
	threads, err := w.store.emailProviderThreads(ctx, append([]int64{anchor.MessageID}, copies...))
	if err != nil {
		return nil, err
	}
	w.threads = threads
	if len(threads) == 0 {
		return found.ids, nil
	}
	conversations := make([]any, 0, len(threads))
	for conversation := range threads {
		conversations = append(conversations, conversation)
	}
	rows, err := w.store.db.QueryContext(ctx, emailFamilyConversationQuery(w.scope, len(conversations), limit+len(w.nodes)+len(found.ids)), append(slices.Clone(w.scopeArgs), conversations...)...)
	if err != nil {
		return nil, fmt.Errorf("find email family conversation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if w.nodes[id] == nil {
			found.add(id)
		}
	}
	return found.ids, rows.Err()
}

// emailFamilyProviderThread reports whether a conversation key is the
// provider's own thread. Any Gmail thread key counts, since a thread's first
// message carries a key equal to its own ID; other sources use the duplicate
// ranking's classification.
func emailFamilyProviderThread(sourceType, sourceMessageID, threadKey string) bool {
	if sourceType == "gmail" {
		return strings.TrimSpace(threadKey) != ""
	}
	return hasProviderThread(sourceType, sourceMessageID, threadKey)
}

// emailProviderThreads returns the conversations of the given emails that are
// the provider's own thread. IMAP, mbox, EML, EMLX, PST and Microsoft Mail
// derive the conversation from message headers, so theirs are left out. A
// conversation a source merge moved keeps its provider key in the merge record.
func (s *Store) emailProviderThreads(ctx context.Context, ids []int64) (map[int64]bool, error) {
	threads := map[int64]bool{}
	err := queryInChunksContext(ctx, s.db, ids, nil,
		`SELECT m.conversation_id, src.source_type, m.source_message_id,
		COALESCE((SELECT original_conversation_id FROM source_merge_conversations smc WHERE smc.conversation_id = c.id), c.source_conversation_id)
		FROM messages m JOIN sources src ON src.id = m.source_id
		JOIN conversations c ON c.id = m.conversation_id WHERE m.id IN (%s)`,
		func(rows *loggedRows) error {
			var conversation int64
			var sourceType string
			var sourceMessageID, threadKey sql.NullString
			if err := rows.Scan(&conversation, &sourceType, &sourceMessageID, &threadKey); err != nil {
				return err
			}
			if emailFamilyProviderThread(sourceType, sourceMessageID.String, threadKey.String) {
				threads[conversation] = true
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("read email family threads: %w", err)
	}
	return threads, nil
}

// emailFamilyPendingQuery asks whether any live email in scope still awaits
// linkage. The predicate is the partial index's own, so the probe stops at its
// first row.
func emailFamilyPendingQuery(scope string) string {
	return `SELECT EXISTS (SELECT 1 FROM messages WHERE deleted_at IS NULL AND ` + emailLinkagePendingPredicate + scope + `)`
}

// emailFamilyConversationQuery reads the given conversations newest first, in
// the order of idx_messages_conversation, so the limit stops the read early.
// The limit counts every row, so callers add the copies they already keep.
func emailFamilyConversationQuery(scope string, conversations, limit int) string {
	return `SELECT m.id FROM messages m WHERE ` + emailFamilyLiveClause + scope +
		` AND m.conversation_id IN (` + strings.TrimSuffix(strings.Repeat("?,", conversations), ",") +
		`) ORDER BY m.sent_at DESC, m.id LIMIT ` + strconv.Itoa(limit)
}

// emailFamilyOwnIDBinds binds identifiers the way the canonical index
// compares them: SQLite compares BLOB bytes, PostgreSQL compares TEXT.
func (s *Store) emailFamilyOwnIDBinds(identifiers []string) []any {
	binds := make([]any, len(identifiers))
	for i, identifier := range identifiers {
		if s.IsPostgreSQL() {
			binds[i] = identifier
		} else {
			binds[i] = []byte(identifier)
		}
	}
	return binds
}

// emailFamilyNodes loads the live emails in scope among ids, with their keys.
func (s *Store) emailFamilyNodes(ctx context.Context, ids []int64, scope string, scopeArgs []any) ([]*emailFamilyNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := map[int64]*emailFamilyNode{}
	var nodes []*emailFamilyNode
	// The draft evidence holds literal percent signs; the template formats %s only.
	query := `SELECT m.id, m.source_id, src.source_type, COALESCE(src.identifier, ''),
		COALESCE(m.account_address, ''), COALESCE(m.account_path, ''), ` + strings.ReplaceAll(draftEvidenceSQL("m", "src"), "%", "%%") + `,
		m.conversation_id, COALESCE(m.subject, ''), m.sent_at, m.email_linkage_outcome,
		` + s.dialect.RFC822CanonicalIDExpr("m.rfc822_message_id") + `
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE ` + emailFamilyLiveClause + scope + ` AND m.id IN (%s) ORDER BY m.id`
	err := queryInChunksContext(ctx, s.db, ids, scopeArgs, query, func(rows *loggedRows) error {
		node := &emailFamilyNode{keys: map[string][]string{}}
		var sent sql.NullTime
		var outcome sql.NullString
		var ownID []byte
		if err := rows.Scan(&node.MessageID, &node.SourceID, &node.SourceType, &node.SourceIdentifier,
			&node.AccountAddress, &node.AccountPath, &node.Draft,
			&node.ConversationID, &node.Subject, &sent, &outcome, &ownID); err != nil {
			return err
		}
		if sent.Valid {
			node.SentAt = &sent.Time
		}
		node.LinkageOutcome = "pending"
		if outcome.Valid {
			node.LinkageOutcome = outcome.String
		}
		node.ownID = string(ownID)
		byID[node.MessageID] = node
		nodes = append(nodes, node)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read email family copies: %w", err)
	}
	err = queryInChunksContext(ctx, s.db, ids, nil,
		`SELECT message_id, kind, value FROM email_link_keys WHERE message_id IN (%s) ORDER BY message_id, kind, value`,
		func(rows *loggedRows) error {
			var id int64
			var kind, value string
			if err := rows.Scan(&id, &kind, &value); err != nil {
				return err
			}
			if node := byID[id]; node != nil {
				node.keys[kind] = append(node.keys[kind], value)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("read email family keys: %w", err)
	}
	return nodes, nil
}

// emailFamilyEvidence names how node relates to the anchor, from the
// identifiers both name. A copy found only through a third copy is indirect.
func emailFamilyEvidence(anchor, node *emailFamilyNode, threads map[int64]bool) string {
	named := func(n *emailFamilyNode, kind, id string) bool {
		return id != "" && slices.Contains(n.keys[kind], id)
	}
	switch {
	case node == anchor:
		return EmailFamilyEvidenceAnchor
	case anchor.ownID != "" && node.ownID == anchor.ownID:
		return EmailFamilyEvidenceCopy
	case named(node, "reply", anchor.ownID):
		return EmailFamilyEvidenceReply
	case named(anchor, "reply", node.ownID):
		return EmailFamilyEvidenceRepliedTo
	case named(node, "attached", anchor.ownID):
		return EmailFamilyEvidenceForward
	case named(anchor, "attached", node.ownID):
		return EmailFamilyEvidenceForwardedOriginal
	case named(node, "reference", anchor.ownID), named(anchor, "reference", node.ownID):
		return EmailFamilyEvidenceReferences
	}
	for _, key := range node.allKeys() {
		if slices.Contains(anchor.allKeys(), key) {
			return EmailFamilyEvidenceReferences
		}
	}
	if threads[node.ConversationID] {
		return EmailFamilyEvidenceThread
	}
	return EmailFamilyEvidenceIndirect
}
