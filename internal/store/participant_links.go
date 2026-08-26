package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// ErrAlreadyLinked is returned by LinkParticipants when the requested edge
// is redundant: the two participants are already connected through other
// links, so adding this edge would create a cycle rather than growing the
// forest.
var ErrAlreadyLinked = errors.New("participants are already linked through other identities")

// ErrParticipantNotFound is returned (wrapped, with the offending IDs in the
// message) by Link/UnlinkParticipants when one or both participant IDs do
// not exist. Callers distinguish it from internal errors via errors.Is so a
// missing row maps to a 400, not a 500.
var ErrParticipantNotFound = errors.New("participant not found")

// ErrInvalidParticipantID is returned (wrapped) by LinkParticipants when the
// two IDs fail the self-link/positive-ID shape check, before any database
// access. Distinguished from internal errors via errors.Is, same as
// ErrParticipantNotFound.
var ErrInvalidParticipantID = errors.New("invalid participant id")

const identityRevisionKey = "identity_revision"

// linkEdge is one row of participant_links, always normalized so a < b.
type linkEdge struct{ a, b int64 }

// rowsScanner is satisfied by both *sql.Rows (non-transactional queries) and
// *loggedRows (queries issued through a *loggedTx), letting store read paths
// share scan routines without depending on one concrete query type.
type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// scanLinkEdges drains rows into a slice of edges, closing rows before
// returning.
func scanLinkEdges(rows rowsScanner) ([]linkEdge, error) {
	defer func() { _ = rows.Close() }()
	var edges []linkEdge
	for rows.Next() {
		var e linkEdge
		if err := rows.Scan(&e.a, &e.b); err != nil {
			return nil, fmt.Errorf("scan participant link: %w", err)
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// loadLinkEdges reads every participant_links row outside of a transaction.
// Used by the read-only cluster resolvers.
func (s *Store) loadLinkEdges() ([]linkEdge, error) {
	return s.loadLinkEdgesContext(context.Background())
}

// loadLinkEdgesContext is the context-aware form of loadLinkEdges.
func (s *Store) loadLinkEdgesContext(ctx context.Context) ([]linkEdge, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT participant_a, participant_b FROM participant_links`)
	if err != nil {
		return nil, fmt.Errorf("query participant links: %w", err)
	}
	return scanLinkEdges(rows)
}

// loadLinkEdgesTx reads every participant_links row within tx. Used by
// Link/UnlinkParticipants so the redundant-edge check sees a consistent
// snapshot with the write that follows it.
func (s *Store) loadLinkEdgesTx(tx *loggedTx) ([]linkEdge, error) {
	return s.loadLinkEdgesTxContext(context.Background(), tx)
}

// loadLinkEdgesTxContext is the context-aware form of loadLinkEdgesTx.
func (s *Store) loadLinkEdgesTxContext(
	ctx context.Context, tx *loggedTx,
) ([]linkEdge, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT participant_a, participant_b FROM participant_links`)
	if err != nil {
		return nil, fmt.Errorf("query participant links: %w", err)
	}
	return scanLinkEdges(rows)
}

// buildAdjacency turns an edge list into an undirected adjacency map. It is a
// package variable rather than a plain func only so tests can count how often
// it runs: the quadratic-cluster fix hinges on ParticipantClusters building
// adjacency exactly once for the whole graph (via clustersFromEdges) instead
// of once per component, and the counting test pins that invariant.
var buildAdjacency = func(edges []linkEdge) map[int64][]int64 {
	adj := make(map[int64][]int64, 2*len(edges))
	for _, e := range edges {
		adj[e.a] = append(adj[e.a], e.b)
		adj[e.b] = append(adj[e.b], e.a)
	}
	return adj
}

// componentOfAdj returns the connected component containing id (including id
// itself), found by breadth-first traversal of an already-built adjacency
// map. An id absent from adj (no edges) returns the single-element set {id}.
// Callers that resolve many components against one graph (ParticipantClusters)
// build adjacency once and call this per root, so the whole pass stays
// O(nodes + edges) rather than rebuilding adjacency for every component.
func componentOfAdj(id int64, adj map[int64][]int64) map[int64]struct{} {
	visited := map[int64]struct{}{id: {}}
	queue := []int64{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if _, seen := visited[next]; !seen {
				visited[next] = struct{}{}
				queue = append(queue, next)
			}
		}
	}
	return visited
}

// componentOf returns the connected component containing id (including id
// itself). It builds adjacency from edges once, then delegates to
// componentOfAdj. Single-lookup callers (ClusterMembers, ClusterEdges, and
// the LinkParticipants connectivity check) resolve one component per call, so
// building adjacency once here is fine. ParticipantClusters, which resolves
// every component, instead shares one adjacency map via componentOfAdj.
func componentOf(id int64, edges []linkEdge) map[int64]struct{} {
	return componentOfAdj(id, buildAdjacency(edges))
}

// normalizeEdge orders a pair so the smaller ID comes first, matching the
// participant_links CHECK (participant_a < participant_b) constraint.
func normalizeEdge(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

// rowQuerier is satisfied by both *loggedDB and *loggedTx, letting revision
// readers share one interface.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type contextRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readIdentityRevision reads the archive_metadata identity revision
// through q (0 if the row does not exist yet).
func readIdentityRevision(q rowQuerier) (int64, error) {
	return scanIdentityRevision(q.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, identityRevisionKey,
	))
}

func readIdentityRevisionContext(ctx context.Context, q contextRowQuerier) (int64, error) {
	return scanIdentityRevision(q.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, identityRevisionKey,
	))
}

func scanIdentityRevision(row scanner) (int64, error) {
	var value string
	err := row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read identity revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse identity revision %q: %w", value, err)
	}
	return revision, nil
}

// IdentityRevision returns the current identity revision (0 if never
// bumped). The revision increments on every link/unlink so callers can
// cheaply detect whether cached cluster data is stale.
func (s *Store) IdentityRevision() (int64, error) {
	return readIdentityRevision(s.db)
}

// currentIdentityRevisionTx reads the revision inside tx without bumping
// it, for idempotent Link/Unlink calls that made no change.
func (s *Store) currentIdentityRevisionTx(tx *loggedTx) (int64, error) {
	return readIdentityRevision(tx)
}

func (s *Store) currentIdentityRevisionTxContext(
	ctx context.Context, tx *loggedTx,
) (int64, error) {
	return readIdentityRevisionContext(ctx, tx)
}

// bumpIdentityRevision increments the revision inside tx and returns the
// new value, seeding the row with 0 first if it does not exist yet.
func (s *Store) bumpIdentityRevision(tx *loggedTx) (int64, error) {
	return s.bumpIdentityRevisionContext(context.Background(), tx)
}

func (s *Store) bumpIdentityRevisionContext(
	ctx context.Context,
	tx *loggedTx,
) (int64, error) {
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		identityRevisionKey); err != nil {
		return 0, fmt.Errorf("seed identity revision: %w", err)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ? RETURNING CAST(value AS INTEGER)`,
		identityRevisionKey).Scan(&revision); err != nil {
		return 0, fmt.Errorf("bump identity revision: %w", err)
	}
	return revision, nil
}

// lockIdentityMutationTx seeds the identity revision row if it does not
// exist yet, then takes a write lock on it. Link/UnlinkParticipants must
// call this before reading the edge snapshot: without it, two concurrent
// calls that each connect previously-disjoint clusters (e.g. link(2,3)
// racing link(1,4) where {1,2} and {3,4} already exist) could both read a
// stale snapshot, both pass the connectivity check, and both commit,
// producing a cycle and breaking the forest invariant documented in
// schema.sql. On SQLite the UPDATE forces the transaction to acquire the
// RESERVED (write) lock immediately, so the edge read that follows is
// serialized against other writers. On PostgreSQL the UPDATE takes a row
// lock on the identity-revision row, so concurrent link/unlink
// transactions queue on it.
//
// ORDERING CONTRACT (PostgreSQL): any transaction that both writes a table
// in exclusiveLockTables and touches the identity-revision row (this lock
// or bumpIdentityRevision) must acquire the row BEFORE its first table
// write. BeginExclusive takes the row and then LOCK TABLE over that list,
// so the reverse order deadlocks against a serialized source removal.
// Transactions with a cheap no-op fast path should check it read-only
// first and take this lock only when they will actually write (see
// SetParticipantIdentifier and the legacy identity migration).
func (s *Store) lockIdentityMutationTx(tx *loggedTx) error {
	return s.lockIdentityMutationTxContext(context.Background(), tx)
}

func (s *Store) lockIdentityMutationTxContext(
	ctx context.Context,
	tx *loggedTx,
) error {
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		identityRevisionKey); err != nil {
		return fmt.Errorf("seed identity revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE archive_metadata SET value = value WHERE key = ?`,
		identityRevisionKey); err != nil {
		return fmt.Errorf("lock identity revision: %w", err)
	}
	return nil
}

// verifyParticipantsExistTx returns a clear ErrParticipantNotFound (wrapped)
// error if either lo or hi is not a participants row, instead of letting the
// caller hit an opaque foreign key violation from the INSERT (LinkParticipants)
// or silently no-op on a nonexistent pair (UnlinkParticipants).
func (s *Store) verifyParticipantsExistTx(tx *loggedTx, lo, hi int64) error {
	return s.verifyParticipantsExistTxContext(context.Background(), tx, lo, hi)
}

func (s *Store) verifyParticipantsExistTxContext(
	ctx context.Context, tx *loggedTx, lo, hi int64,
) error {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM participants WHERE id IN (?, ?)`, lo, hi,
	).Scan(&count); err != nil {
		return fmt.Errorf("verify participants exist: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("participant ids must exist (got %d, %d): %w", lo, hi, ErrParticipantNotFound)
	}
	return nil
}

// LinkParticipants asserts a and b are the same person. Returns
// ErrInvalidParticipantID (wrapped) for a self-link or non-positive ID, and
// ErrParticipantNotFound (wrapped) if either ID is not a participants row.
// Idempotent for the exact existing edge; returns ErrAlreadyLinked for a new
// redundant edge between participants already connected indirectly. Linking
// clusters curated as different durable people returns
// ErrPersonBindingConflict (wrapped) rather than merging those profiles.
// When exactly one durable person covers the two clusters, the new link
// binds the combined cluster's unbound members to that person (bumping its
// revision), so person membership never drifts behind cluster membership.
// Returns the identity revision after the call.
func (s *Store) LinkParticipants(a, b int64) (int64, error) {
	revision, _, err := s.linkParticipantsContext(context.Background(), a, b)
	return revision, err
}

// linkParticipantsContext is the shared participant-link transaction. The
// boolean reports whether this call inserted a new edge.
func (s *Store) linkParticipantsContext(
	ctx context.Context, a, b int64,
) (revision int64, linked bool, err error) {
	return s.linkParticipantsContextGuarded(ctx, a, b, nil)
}

func (s *Store) linkParticipantsContextGuarded(
	ctx context.Context,
	a, b int64,
	guard func(context.Context, *loggedTx) error,
) (revision int64, linked bool, err error) {
	return s.linkParticipantsContextGuardedOwned(ctx, a, b, 0, guard)
}

// linkParticipantsContextGuardedOwned is the guarded link transaction with an
// optional identity-match-candidate owner. A zero owner preserves the
// ordinary user/import link path. The owner is written with the edge itself,
// under the identity mutation lock, so a later system rejection can
// distinguish an edge this candidate inserted from a pre-existing manual edge.
func (s *Store) linkParticipantsContextGuardedOwned(
	ctx context.Context,
	a, b, ownerCandidateID int64,
	guard func(context.Context, *loggedTx) error,
) (revision int64, linked bool, err error) {
	if a == b || a <= 0 || b <= 0 {
		return 0, false, fmt.Errorf("link participants: ids must be distinct positive IDs (got %d, %d): %w",
			a, b, ErrInvalidParticipantID)
	}
	lo, hi := normalizeEdge(a, b)

	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		if err := s.verifyParticipantsExistTxContext(ctx, tx, lo, hi); err != nil {
			return err
		}
		edges, err := s.loadLinkEdgesTxContext(ctx, tx)
		if err != nil {
			return err
		}
		personID, unionMembers, err := s.personForClusterUnionTx(
			ctx, tx, lo, hi, edges,
		)
		if err != nil {
			return err
		}
		for _, e := range edges {
			if e.a == lo && e.b == hi {
				if ownerCandidateID == 0 {
					result, updateErr := tx.ExecContext(ctx, `UPDATE participant_links
						SET identity_match_candidate_id = NULL
						WHERE participant_a = ? AND participant_b = ?
						  AND identity_match_candidate_id IS NOT NULL`, lo, hi)
					if updateErr != nil {
						return fmt.Errorf("confirm participant link: %w", updateErr)
					}
					changed, updateErr := result.RowsAffected()
					if updateErr != nil {
						return fmt.Errorf("count confirmed participant link: %w", updateErr)
					}
					if changed > 0 {
						revision, updateErr = s.bumpIdentityRevisionContext(ctx, tx)
						return updateErr
					}
				}
				revision, err = s.currentIdentityRevisionTxContext(ctx, tx)
				return err
			}
		}
		if _, connected := componentOf(lo, edges)[hi]; connected {
			// An accepted identity assertion that is already satisfied through
			// another path has completed its application work. Its guard may
			// have cleared durable recovery state, so commit that update while
			// preserving ErrAlreadyLinked for ordinary manual link callers.
			if ownerCandidateID > 0 {
				revision, err = s.currentIdentityRevisionTxContext(ctx, tx)
				return err
			}
			return ErrAlreadyLinked
		}
		var insertErr error
		if ownerCandidateID > 0 {
			_, insertErr = tx.ExecContext(ctx,
				`INSERT INTO participant_links
				 (participant_a, participant_b, identity_match_candidate_id)
				 VALUES (?, ?, ?)`, lo, hi, ownerCandidateID)
		} else {
			_, insertErr = tx.ExecContext(ctx,
				`INSERT INTO participant_links (participant_a, participant_b)
				 VALUES (?, ?)`, lo, hi)
		}
		if insertErr != nil {
			return fmt.Errorf("insert participant link: %w", insertErr)
		}
		if personID != 0 {
			if err := extendActivePersonMergeLineageTx(
				ctx, tx, personID, lo, hi, edges,
			); err != nil {
				return err
			}
			changed, err := s.bindPersonParticipantsTx(
				ctx, tx, personID, unionMembers)
			if err != nil {
				return err
			}
			if changed {
				if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
					return err
				}
			}
		}
		revision, err = s.bumpIdentityRevisionContext(ctx, tx)
		if err == nil {
			linked = true
			if personID != 0 {
				err = s.publishPersonIdentityScopeChangesTx(
					ctx, tx, []int64{personID},
					peoplesweep.EvidenceEffectIdentityReassigned)
			}
		}
		return err
	})
	return revision, linked, err
}

type activePersonMergeLineageState struct {
	origin  string
	splitID sql.NullInt64
}

// extendActivePersonMergeLineageTx keeps reversible merge lineage aligned
// with identity links added after the merge. Each pre-link component inherits
// its one unambiguous state. A lineage-free component inherits the state of
// the participant it is directly linked to, which remains deterministic even
// when that participant's existing component contains both merge origins.
func extendActivePersonMergeLineageTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	lo, hi int64,
	edges []linkEdge,
) error {
	left := sortedComponentMembers(lo, edges)
	right := sortedComponentMembers(hi, edges)
	members := make(map[int64]struct{}, len(left)+len(right))
	for _, participantID := range append(slices.Clone(left), right...) {
		members[participantID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT lineage.merge_id,
		lineage.participant_id, lineage.origin_side, lineage.split_id
		FROM person_merge_participants lineage
		JOIN person_merges merge_record ON merge_record.id = lineage.merge_id
		WHERE merge_record.current_person_id = ?
		ORDER BY lineage.merge_id, lineage.participant_id`, personID)
	if err != nil {
		return fmt.Errorf("load active person merge lineage for link: %w", err)
	}
	defer func() { _ = rows.Close() }()
	lineage := make(map[int64]map[int64]activePersonMergeLineageState)
	for rows.Next() {
		var mergeID, participantID int64
		var state activePersonMergeLineageState
		if err := rows.Scan(&mergeID, &participantID, &state.origin, &state.splitID); err != nil {
			return fmt.Errorf("scan active person merge lineage for link: %w", err)
		}
		if _, included := members[participantID]; !included {
			continue
		}
		if lineage[mergeID] == nil {
			lineage[mergeID] = make(map[int64]activePersonMergeLineageState)
		}
		lineage[mergeID][participantID] = state
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active person merge lineage for link: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close active person merge lineage for link: %w", err)
	}
	mergeIDs := make([]int64, 0, len(lineage))
	for mergeID := range lineage {
		mergeIDs = append(mergeIDs, mergeID)
	}
	slices.Sort(mergeIDs)
	for _, mergeID := range mergeIDs {
		mergeLineage := lineage[mergeID]
		leftState, leftPresent, leftUnambiguous := activePersonMergeComponentState(
			left, mergeLineage,
		)
		rightState, rightPresent, rightUnambiguous := activePersonMergeComponentState(
			right, mergeLineage,
		)
		assignments := make(map[int64]activePersonMergeLineageState)
		if leftUnambiguous {
			assignPersonMergeComponentLineage(assignments, left, leftState)
		}
		if rightUnambiguous {
			assignPersonMergeComponentLineage(assignments, right, rightState)
		}
		if !leftPresent {
			state, found := mergeLineage[hi]
			if !found && rightUnambiguous {
				state, found = rightState, true
			}
			if found {
				assignPersonMergeComponentLineage(assignments, left, state)
			}
		}
		if !rightPresent {
			state, found := mergeLineage[lo]
			if !found && leftUnambiguous {
				state, found = leftState, true
			}
			if found {
				assignPersonMergeComponentLineage(assignments, right, state)
			}
		}
		participantIDs := make([]int64, 0, len(assignments))
		for participantID := range assignments {
			participantIDs = append(participantIDs, participantID)
		}
		slices.Sort(participantIDs)
		for _, participantID := range participantIDs {
			state := assignments[participantID]
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_participants
				(merge_id, participant_id, origin_side, split_id)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (merge_id, participant_id) DO NOTHING`,
				mergeID, participantID, state.origin, state.splitID); err != nil {
				return fmt.Errorf("extend person merge lineage for linked participant: %w", err)
			}
		}
	}
	return nil
}

func activePersonMergeComponentState(
	component []int64,
	lineage map[int64]activePersonMergeLineageState,
) (activePersonMergeLineageState, bool, bool) {
	var state activePersonMergeLineageState
	present := false
	for _, participantID := range component {
		current, found := lineage[participantID]
		if !found {
			continue
		}
		if !present {
			state, present = current, true
			continue
		}
		if state != current {
			return activePersonMergeLineageState{}, true, false
		}
	}
	return state, present, present
}

func assignPersonMergeComponentLineage(
	assignments map[int64]activePersonMergeLineageState,
	component []int64,
	state activePersonMergeLineageState,
) {
	for _, participantID := range component {
		assignments[participantID] = state
	}
}

// UnlinkParticipants removes the edge between a and b, if present. Returns
// ErrParticipantNotFound (wrapped) if either ID is not a participants row.
// Idempotent: unlinking a pair with no edge is a no-op that returns the
// current revision unchanged. Returns the identity revision after the call.
func (s *Store) UnlinkParticipants(a, b int64) (int64, error) {
	lo, hi := normalizeEdge(a, b)

	var revision int64
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		if err := s.verifyParticipantsExistTx(tx, lo, hi); err != nil {
			return err
		}
		edges, err := s.loadLinkEdgesTx(tx)
		if err != nil {
			return err
		}
		found := false
		for _, edge := range edges {
			if edge.a == lo && edge.b == hi {
				found = true
				break
			}
		}
		if !found {
			revision, err = s.currentIdentityRevisionTx(tx)
			return err
		}
		affectedMembers := sortedComponentMembers(lo, edges)
		affectedPeople, err := s.trackedPersonIDsForParticipantsTx(
			context.Background(), tx, affectedMembers)
		if err != nil {
			return err
		}
		res, err := tx.Exec(
			`DELETE FROM participant_links WHERE participant_a = ? AND participant_b = ?`,
			lo, hi)
		if err != nil {
			return fmt.Errorf("delete participant link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		if n == 0 {
			revision, err = s.currentIdentityRevisionTx(tx)
			return err
		}
		if err := s.rejectAcceptedIdentityMatchesAcrossUnlinkTx(
			tx, lo, hi, edges,
		); err != nil {
			return err
		}
		revision, err = s.bumpIdentityRevision(tx)
		if err != nil {
			return err
		}
		return s.publishPersonIdentityScopeChangesTx(
			context.Background(), tx, affectedPeople,
			peoplesweep.EvidenceEffectIdentityReassigned)
	})
	return revision, err
}

// rejectAcceptedIdentityMatchesAcrossUnlinkTx suppresses every accepted
// participant candidate whose endpoints would cross the two components made
// by removing (a, b). The direct edge owner is not sufficient: another
// accepted candidate may have observed the pair already connected and can
// otherwise recreate the edge during accepted-match recovery. The candidate
// scan is restricted to the component that contained the removed edge, and
// the caller already holds the identity-mutation lock.
func (s *Store) rejectAcceptedIdentityMatchesAcrossUnlinkTx(
	tx *loggedTx, a, b int64, edges []linkEdge,
) error {
	original := componentOf(a, edges)
	if _, ok := original[b]; !ok {
		return nil
	}
	remaining := make([]linkEdge, 0, len(edges)-1)
	removed := false
	for _, edge := range edges {
		if edge.a == a && edge.b == b {
			removed = true
			continue
		}
		remaining = append(remaining, edge)
	}
	if !removed {
		return nil
	}
	left := componentOf(a, remaining)
	right := componentOf(b, remaining)
	if _, stillConnected := left[b]; stillConnected {
		return nil
	}

	// Scan accepted participant candidates without interpolating the component
	// into an IN list: a large identity cluster can exceed SQLite or PostgreSQL
	// bind-parameter limits. The component and split checks below keep the
	// result bounded to the original cluster in memory.
	rows, err := tx.Query(`
		SELECT id, left_id, right_id
		FROM identity_match_candidates
		WHERE state = ? AND left_kind = ? AND right_kind = ?`,
		IdentityMatchStateAccepted,
		IdentityMatchParticipant,
		IdentityMatchParticipant,
	)
	if err != nil {
		return fmt.Errorf("find accepted identity matches crossing unlink: %w", err)
	}
	candidateIDs := make([]int64, 0)
	for rows.Next() {
		var candidateID, leftID, rightID int64
		if err := rows.Scan(&candidateID, &leftID, &rightID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan accepted identity match crossing unlink: %w", err)
		}
		_, leftInOriginal := original[leftID]
		_, rightInOriginal := original[rightID]
		if !leftInOriginal || !rightInOriginal {
			continue
		}
		_, leftInLeft := left[leftID]
		_, leftInRight := right[leftID]
		_, rightInLeft := left[rightID]
		_, rightInRight := right[rightID]
		if (leftInLeft && rightInRight) || (leftInRight && rightInLeft) {
			candidateIDs = append(candidateIDs, candidateID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate accepted identity matches crossing unlink: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close accepted identity matches crossing unlink: %w", err)
	}
	if len(candidateIDs) == 0 {
		return nil
	}
	return s.rejectAcceptedIdentityMatchCandidatesTx(
		context.Background(), tx, candidateIDs, "crossing unlink")
}

func (s *Store) rejectAcceptedIdentityMatchesAcrossPersonSplitTx(
	ctx context.Context, tx *loggedTx, selected []int64,
) error {
	selectedSet := make(map[int64]struct{}, len(selected))
	for _, participantID := range selected {
		selectedSet[participantID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, left_id, right_id
		FROM identity_match_candidates
		WHERE state = ? AND left_kind = ? AND right_kind = ?`,
		IdentityMatchStateAccepted,
		IdentityMatchParticipant,
		IdentityMatchParticipant,
	)
	if err != nil {
		return fmt.Errorf("find accepted identity matches crossing person split: %w", err)
	}
	candidateIDs := make([]int64, 0)
	for rows.Next() {
		var candidateID, leftID, rightID int64
		if err := rows.Scan(&candidateID, &leftID, &rightID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan accepted identity match crossing person split: %w", err)
		}
		_, leftSelected := selectedSet[leftID]
		_, rightSelected := selectedSet[rightID]
		if leftSelected != rightSelected {
			candidateIDs = append(candidateIDs, candidateID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate accepted identity matches crossing person split: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close accepted identity matches crossing person split: %w", err)
	}
	return s.rejectAcceptedIdentityMatchCandidatesTx(
		ctx, tx, candidateIDs, "crossing person split")
}

func (s *Store) rejectAcceptedIdentityMatchCandidatesTx(
	ctx context.Context,
	tx *loggedTx,
	candidateIDs []int64,
	operation string,
) error {
	for _, candidateID := range candidateIDs {
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_candidates SET
				state = ?, decided_by = ?, decided_at = `+s.dialect.Now()+`,
				pre_conflict_state = NULL, application_pending = FALSE,
				updated_at = `+s.dialect.Now()+`
			WHERE state = ? AND id = ?`,
			IdentityMatchStateRejected, "user", IdentityMatchStateAccepted,
			candidateID,
		); err != nil {
			return fmt.Errorf("reject identity match %d %s: %w", candidateID, operation, err)
		}
	}
	return nil
}

// rewriteLinksForMerge repoints link edges from loser to winner when a
// participant merge (MergeParticipants, mergeParticipant) absorbs loser into
// winner. Must run inside the merge's own transaction, before the final
// `DELETE FROM participants WHERE id = ?`: participant_links has an ON
// DELETE CASCADE FK, and letting that cascade fire first would silently
// drop the user's links instead of repointing them.
//
// A plain repoint is not enough: contracting the two endpoints of a path can
// create a cycle. Links a-x, x-y, y-b form a path; merging b into a
// collapses the path's endpoints together, and repointing y-b to y-a alone
// would yield the cycle a-x-y-a. So instead of repointing in place, this
// rebuilds the affected cluster as a cycle-free spanning tree of the remapped
// original edges. Keeping those edges, rather than inventing a canonical
// shape, gives every rebuilt edge real manual or candidate provenance.
//
// Both callers (MergeParticipants, mergeParticipant) bump the identity
// revision unconditionally after calling this, regardless of whether it
// touched any edge: a merge can change owner_participants even when it
// touches no link edge, so there is no return value for them to condition
// on.
func (s *Store) rewriteLinksForMerge(tx *loggedTx, loser, winner int64) error {
	return s.rewriteLinksForMergeContext(context.Background(), tx, loser, winner)
}

// rewriteLinksForMergeContext is the context-aware form of
// rewriteLinksForMerge. The legacy phone-unique migration uses it: its merge
// runs inside a maintenance transaction with the pool-wide statement_timeout
// disabled, so on PostgreSQL nothing but ctx can cut short a statement here
// that is waiting on a conflicting lock.
func (s *Store) rewriteLinksForMergeContext(
	ctx context.Context, tx *loggedTx, loser, winner int64,
) error {
	if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
		return err
	}
	edges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return err
	}
	if !linksReference(edges, loser, winner) {
		return nil
	}

	members := mergedClusterMembers(loser, winner, edges)
	if len(members) < 2 {
		// loser and winner were only ever linked to each other: now that
		// they are literally the same row, the edge is redundant and
		// simply disappears rather than being replaced by a self-loop.
		return deleteMergeEdge(ctx, tx, loser, winner)
	}
	owners, err := loadParticipantLinkOwnersTxContext(ctx, tx)
	if err != nil {
		return err
	}
	return rebuildClusterAsSpanningTree(ctx, tx, members, owners, loser, winner)
}

// linksReference reports whether any edge in edges has loser or winner as
// an endpoint.
func linksReference(edges []linkEdge, loser, winner int64) bool {
	for _, e := range edges {
		if e.a == loser || e.b == loser || e.a == winner || e.b == winner {
			return true
		}
	}
	return false
}

// mergedClusterMembers returns the node set of the cluster that must be
// rebuilt after the merge: the combined reach of loser's and winner's
// components before contraction, with loser replaced by winner.
func mergedClusterMembers(loser, winner int64, edges []linkEdge) map[int64]struct{} {
	members := componentOf(loser, edges)
	for m := range componentOf(winner, edges) {
		members[m] = struct{}{}
	}
	delete(members, loser)
	members[winner] = struct{}{}
	return members
}

// deleteMergeEdge removes the edge between loser and winner. Used when they
// were only ever linked to each other, so the merge leaves no cluster to
// rebuild.
func deleteMergeEdge(ctx context.Context, tx *loggedTx, loser, winner int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM participant_links WHERE participant_a IN (?, ?) OR participant_b IN (?, ?)`,
		loser, winner, loser, winner,
	); err != nil {
		return fmt.Errorf("delete direct merge link: %w", err)
	}
	return nil
}

type participantLinkOwner struct {
	a, b  int64
	owner int64
}

func loadParticipantLinkOwnersTxContext(
	ctx context.Context, tx *loggedTx,
) ([]participantLinkOwner, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT participant_a, participant_b,
			COALESCE(identity_match_candidate_id, 0)
		 FROM participant_links`)
	if err != nil {
		return nil, fmt.Errorf("query participant link ownership: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make([]participantLinkOwner, 0)
	for rows.Next() {
		var owner participantLinkOwner
		if err := rows.Scan(&owner.a, &owner.b, &owner.owner); err != nil {
			return nil, fmt.Errorf("scan participant link ownership: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query participant link ownership: %w", err)
	}
	return owners, nil
}

// rebuildClusterAsSpanningTree deletes every link edge touching a member of
// the cluster, then reinserts a deterministic spanning tree selected from the
// remapped original edges. Manual support wins when old edges collapse;
// otherwise the lowest candidate ID owns the edge. Because no replacement
// edge is invented, every rebuilt edge retains real provenance.
func rebuildClusterAsSpanningTree(
	ctx context.Context, tx *loggedTx, members map[int64]struct{},
	owners []participantLinkOwner, loser, winner int64,
) error {
	ids := make([]int64, 0, len(members))
	for m := range members {
		ids = append(ids, m)
	}
	slices.Sort(ids)

	contributions := mergeParticipantLinkOwners(owners, loser, winner)
	newEdges := participantLinkSpanningTree(members, contributions)
	if len(newEdges) != len(ids)-1 {
		return errors.New("rebuild merged participant cluster: remapped links are disconnected")
	}
	if err := deleteClusterEdges(ctx, tx, ids); err != nil {
		return err
	}

	for _, edge := range newEdges {
		var insertErr error
		if owner := contributions[edge]; owner != 0 {
			_, insertErr = tx.ExecContext(ctx,
				`INSERT INTO participant_links
				 (participant_a, participant_b, identity_match_candidate_id)
				 VALUES (?, ?, ?)`, edge[0], edge[1], owner)
		} else {
			_, insertErr = tx.ExecContext(ctx,
				`INSERT INTO participant_links (participant_a, participant_b)
				 VALUES (?, ?)`, edge[0], edge[1])
		}
		if insertErr != nil {
			return fmt.Errorf("insert merged participant link: %w", insertErr)
		}
	}
	return nil
}

func mergeParticipantLinkOwners(
	owners []participantLinkOwner,
	loser, winner int64,
) map[[2]int64]int64 {
	result := make(map[[2]int64]int64)
	manual := make(map[[2]int64]struct{})
	for _, old := range owners {
		a, b := old.a, old.b
		if a == loser {
			a = winner
		}
		if b == loser {
			b = winner
		}
		if a == b {
			continue
		}
		a, b = normalizeEdge(a, b)
		edge := [2]int64{a, b}
		if old.owner == 0 {
			result[edge] = 0
			manual[edge] = struct{}{}
			continue
		}
		if _, manualEdge := manual[edge]; manualEdge {
			continue
		}
		if prior, exists := result[edge]; !exists || old.owner < prior {
			result[edge] = old.owner
		}
	}
	return result
}

func participantLinkSpanningTree(
	members map[int64]struct{},
	contributions map[[2]int64]int64,
) [][2]int64 {
	edges := make([][2]int64, 0, len(contributions))
	for edge := range contributions {
		if _, ok := members[edge[0]]; !ok {
			continue
		}
		if _, ok := members[edge[1]]; !ok {
			continue
		}
		edges = append(edges, edge)
	}
	slices.SortFunc(edges, func(a, b [2]int64) int {
		aManual, bManual := contributions[a] == 0, contributions[b] == 0
		if aManual != bManual {
			if aManual {
				return -1
			}
			return 1
		}
		if a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	})

	parent := make(map[int64]int64, len(members))
	for member := range members {
		parent[member] = member
	}
	var find func(int64) int64
	find = func(node int64) int64 {
		if parent[node] != node {
			parent[node] = find(parent[node])
		}
		return parent[node]
	}
	tree := make([][2]int64, 0, len(members)-1)
	for _, edge := range edges {
		aRoot, bRoot := find(edge[0]), find(edge[1])
		if aRoot == bRoot {
			continue
		}
		parent[bRoot] = aRoot
		tree = append(tree, edge)
	}
	return tree
}

// deleteClusterEdges removes every link edge with an endpoint in ids. Any
// edge touching a merge's loser has its other endpoint in ids (that
// endpoint is in componentOf(loser) by construction), so this also removes
// the loser's edges without the loser needing to appear in ids.
func deleteClusterEdges(ctx context.Context, tx *loggedTx, ids []int64) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, 2*len(ids))
	for range 2 {
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM participant_links WHERE participant_a IN (%s) OR participant_b IN (%s)`,
		placeholders, placeholders,
	), args...); err != nil {
		return fmt.Errorf("delete affected cluster links: %w", err)
	}
	return nil
}

// clustersFromEdges maps each participant that appears in an edge to its
// cluster's canonical ID (the smallest member). It builds adjacency once for
// the whole graph and visits each node and edge once across all components,
// so the whole pass is O(nodes + edges) regardless of how many disconnected
// components the edge list contains.
func clustersFromEdges(edges []linkEdge) map[int64]int64 {
	adj := buildAdjacency(edges)

	ids := make([]int64, 0, len(adj))
	for id := range adj {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	clusters := make(map[int64]int64, len(adj))
	visited := make(map[int64]struct{}, len(adj))
	for _, id := range ids {
		if _, seen := visited[id]; seen {
			continue
		}
		// Ascending traversal guarantees id is the smallest unvisited
		// node overall, and therefore the smallest member of its own
		// component: any smaller member would already be visited,
		// either as an earlier component's root or reached via BFS
		// from one. Traversing the shared adj keeps the whole pass
		// linear instead of rebuilding adjacency for each component.
		for member := range componentOfAdj(id, adj) {
			clusters[member] = id
			visited[member] = struct{}{}
		}
	}
	return clusters
}

// ParticipantClusters returns participant_id → canonical cluster ID
// (the smallest member ID) for every participant that appears in a link
// edge. Unlinked participants are their own cluster and are not returned.
func (s *Store) ParticipantClusters() (map[int64]int64, error) {
	edges, err := s.loadLinkEdges()
	if err != nil {
		return nil, err
	}
	return clustersFromEdges(edges), nil
}

// ClusterMembers returns all participant IDs in the cluster containing id
// (including id itself), sorted ascending. Single-element for unlinked ids.
func (s *Store) ClusterMembers(id int64) ([]int64, error) {
	edges, err := s.loadLinkEdges()
	if err != nil {
		return nil, err
	}
	component := componentOf(id, edges)
	members := make([]int64, 0, len(component))
	for member := range component {
		members = append(members, member)
	}
	slices.Sort(members)
	return members, nil
}

// LinkEdge is one participant_links row, exposed to API callers (the
// person-detail HTTP handler) that need the literal edges of a cluster, not
// just its membership — e.g. to render a per-chip unlink affordance that
// calls UnlinkParticipants with an exact existing edge.
type LinkEdge struct {
	A int64 `json:"participant_a"`
	B int64 `json:"participant_b"`
}

// ClusterEdges returns every link edge in the connected component containing
// id (including id itself), normalized a<b as stored. Empty for an unlinked
// id. Order is unspecified; callers that need a stable order should sort.
func (s *Store) ClusterEdges(id int64) ([]LinkEdge, error) {
	edges, err := s.loadLinkEdges()
	if err != nil {
		return nil, err
	}
	component := componentOf(id, edges)
	result := make([]LinkEdge, 0, len(edges))
	for _, e := range edges {
		if _, ok := component[e.a]; ok {
			result = append(result, LinkEdge{A: e.a, B: e.b})
		}
	}
	return result, nil
}
