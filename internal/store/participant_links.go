package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

// edgeRows is satisfied by both *sql.Rows (non-transactional queries) and
// *loggedRows (queries issued through a *loggedTx), letting loadLinkEdges
// and loadLinkEdgesTx share one scan routine.
type edgeRows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// scanLinkEdges drains rows into a slice of edges, closing rows before
// returning.
func scanLinkEdges(rows edgeRows) ([]linkEdge, error) {
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
	rows, err := s.db.Query(`SELECT participant_a, participant_b FROM participant_links`)
	if err != nil {
		return nil, fmt.Errorf("query participant links: %w", err)
	}
	return scanLinkEdges(rows)
}

// loadLinkEdgesTx reads every participant_links row within tx. Used by
// Link/UnlinkParticipants so the redundant-edge check sees a consistent
// snapshot with the write that follows it.
func (s *Store) loadLinkEdgesTx(tx *loggedTx) ([]linkEdge, error) {
	rows, err := tx.Query(`SELECT participant_a, participant_b FROM participant_links`)
	if err != nil {
		return nil, fmt.Errorf("query participant links: %w", err)
	}
	return scanLinkEdges(rows)
}

// buildAdjacency turns an edge list into an undirected adjacency map.
func buildAdjacency(edges []linkEdge) map[int64][]int64 {
	adj := make(map[int64][]int64, 2*len(edges))
	for _, e := range edges {
		adj[e.a] = append(adj[e.a], e.b)
		adj[e.b] = append(adj[e.b], e.a)
	}
	return adj
}

// componentOf returns the connected component containing id (including id
// itself), found by breadth-first traversal of edges. An id with no edges
// returns the single-element set {id}.
func componentOf(id int64, edges []linkEdge) map[int64]struct{} {
	adj := buildAdjacency(edges)
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

// normalizeEdge orders a pair so the smaller ID comes first, matching the
// participant_links CHECK (participant_a < participant_b) constraint.
func normalizeEdge(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

// rowQuerier is satisfied by both *loggedDB and *loggedTx, letting
// readIdentityRevision serve both the non-transactional IdentityRevision
// and the in-transaction currentIdentityRevisionTx from one implementation.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// readIdentityRevision reads the archive_metadata identity revision
// through q (0 if the row does not exist yet).
func readIdentityRevision(q rowQuerier) (int64, error) {
	var value string
	err := q.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, identityRevisionKey,
	).Scan(&value)
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

// bumpIdentityRevision increments the revision inside tx and returns the
// new value, seeding the row with 0 first if it does not exist yet.
func (s *Store) bumpIdentityRevision(tx *loggedTx) (int64, error) {
	if _, err := tx.Exec(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		identityRevisionKey); err != nil {
		return 0, fmt.Errorf("seed identity revision: %w", err)
	}
	var revision int64
	if err := tx.QueryRow(
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
func (s *Store) lockIdentityMutationTx(tx *loggedTx) error {
	if _, err := tx.Exec(s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		identityRevisionKey); err != nil {
		return fmt.Errorf("seed identity revision: %w", err)
	}
	if _, err := tx.Exec(
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
	var count int
	if err := tx.QueryRow(
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
// redundant edge between participants already connected indirectly. Returns
// the identity revision after the call.
func (s *Store) LinkParticipants(a, b int64) (int64, error) {
	if a == b || a <= 0 || b <= 0 {
		return 0, fmt.Errorf("link participants: ids must be distinct positive IDs (got %d, %d): %w",
			a, b, ErrInvalidParticipantID)
	}
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
		for _, e := range edges {
			if e.a == lo && e.b == hi {
				revision, err = s.currentIdentityRevisionTx(tx)
				return err
			}
		}
		if _, connected := componentOf(lo, edges)[hi]; connected {
			return ErrAlreadyLinked
		}
		if _, err := tx.Exec(
			`INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?)`,
			lo, hi); err != nil {
			return fmt.Errorf("insert participant link: %w", err)
		}
		revision, err = s.bumpIdentityRevision(tx)
		return err
	})
	return revision, err
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
		revision, err = s.bumpIdentityRevision(tx)
		return err
	})
	return revision, err
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
// rebuilds the entire affected cluster as a canonical star rooted at its
// smallest member ID, which is always cycle-free.
//
// Both callers (MergeParticipants, mergeParticipant) bump the identity
// revision unconditionally after calling this, regardless of whether it
// touched any edge: a merge can change owner_participants even when it
// touches no link edge, so there is no return value for them to condition
// on.
func (s *Store) rewriteLinksForMerge(tx *loggedTx, loser, winner int64) error {
	if err := s.lockIdentityMutationTx(tx); err != nil {
		return err
	}
	edges, err := s.loadLinkEdgesTx(tx)
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
		return deleteMergeEdge(tx, loser, winner)
	}
	return rebuildClusterAsStar(tx, members)
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
func deleteMergeEdge(tx *loggedTx, loser, winner int64) error {
	if _, err := tx.Exec(
		`DELETE FROM participant_links WHERE participant_a IN (?, ?) OR participant_b IN (?, ?)`,
		loser, winner, loser, winner,
	); err != nil {
		return fmt.Errorf("delete direct merge link: %w", err)
	}
	return nil
}

// rebuildClusterAsStar deletes every link edge touching a member of the
// cluster, then reinserts a canonical star rooted at the smallest member
// ID — the one shape vertex contraction can never turn into a cycle.
func rebuildClusterAsStar(tx *loggedTx, members map[int64]struct{}) error {
	ids := make([]int64, 0, len(members))
	for m := range members {
		ids = append(ids, m)
	}
	slices.Sort(ids)

	if err := deleteClusterEdges(tx, ids); err != nil {
		return err
	}

	canonical := ids[0]
	for _, other := range ids[1:] {
		lo, hi := normalizeEdge(canonical, other)
		if _, err := tx.Exec(
			`INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?)`,
			lo, hi,
		); err != nil {
			return fmt.Errorf("insert canonical star link: %w", err)
		}
	}
	return nil
}

// deleteClusterEdges removes every link edge with an endpoint in ids. Any
// edge touching a merge's loser has its other endpoint in ids (that
// endpoint is in componentOf(loser) by construction), so this also removes
// the loser's edges without the loser needing to appear in ids.
func deleteClusterEdges(tx *loggedTx, ids []int64) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, 2*len(ids))
	for range 2 {
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM participant_links WHERE participant_a IN (%s) OR participant_b IN (%s)`,
		placeholders, placeholders,
	), args...); err != nil {
		return fmt.Errorf("delete affected cluster links: %w", err)
	}
	return nil
}

// ParticipantClusters returns participant_id → canonical cluster ID
// (the smallest member ID) for every participant that appears in a link
// edge. Unlinked participants are their own cluster and are not returned.
func (s *Store) ParticipantClusters() (map[int64]int64, error) {
	edges, err := s.loadLinkEdges()
	if err != nil {
		return nil, err
	}
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
		// from one.
		for member := range componentOf(id, edges) {
			clusters[member] = id
			visited[member] = struct{}{}
		}
	}
	return clusters, nil
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
