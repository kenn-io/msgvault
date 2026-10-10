package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

const maxIdentityOperationMembers = 100

var ErrIdentityOperationTooLarge = errors.New("identity operation exceeds 100 affected participants")

// IdentitySnapshot is complete authorization and revision evidence for one
// explicit operation. ComponentMembers is the binding effect; Members also
// includes the memberships of every affected durable person.
type IdentitySnapshot struct {
	Operation                identitycontrol.Operation      `json:"operation"`
	Target                   identitycontrol.IdentityTarget `json:"target"`
	IdentityRevision         int64                          `json:"identity_revision"`
	ComponentMembers         []int64                        `json:"component_members"`
	OtherComponentMembers    []int64                        `json:"other_component_members"`
	Members                  []int64                        `json:"members"`
	Participants             []IdentityParticipantEvidence  `json:"participants"`
	Persons                  []IdentityPersonEvidence       `json:"persons"`
	Bindings                 []IdentityBindingEvidence      `json:"bindings"`
	Links                    []IdentityLinkEvidence         `json:"links"`
	Candidates               []IdentityCandidateEvidence    `json:"candidates"`
	Sources                  []IdentitySourceEvidence       `json:"sources"`
	SourceContributions      []IdentitySourceContribution   `json:"source_contributions"`
	Accounts                 []IdentityAccountEvidence      `json:"accounts"`
	Books                    []IdentityBookEvidence         `json:"books"`
	Resources                []IdentityResourceEvidence     `json:"resources"`
	Conflicts                []IdentityConflictEvidence     `json:"conflicts"`
	Publications             []IdentityPublicationEvidence  `json:"publications"`
	Lineages                 []IdentityLineageEvidence      `json:"lineages"`
	ActiveMergeIDs           []int64                        `json:"active_merge_ids"`
	Blockers                 []string                       `json:"blockers"`
	ManualConfirmCandidateID int64                          `json:"manual_confirm_candidate_id,omitzero"`
	Noop                     bool                           `json:"noop"`
	Fingerprint              string                         `json:"fingerprint"`
}

type IdentityPersonEvidence struct {
	ID                 int64  `json:"id"`
	UID                string `json:"uid"`
	Revision           int64  `json:"revision"`
	ProjectionRevision int64  `json:"projection_revision"`
}

type IdentityParticipantEvidence struct {
	ID                     int64  `json:"id"`
	DisplayName            string `json:"display_name"`
	Email                  string `json:"email"`
	Phone                  string `json:"phone"`
	IdentifierFingerprint  string `json:"identifier_fingerprint"`
	ObservationFingerprint string `json:"observation_fingerprint"`
}
type IdentityBindingEvidence struct {
	ParticipantID int64 `json:"participant_id"`
	PersonID      int64 `json:"person_id"`
}
type IdentityLinkEvidence struct {
	ParticipantID      int64 `json:"participant_id"`
	OtherParticipantID int64 `json:"other_participant_id"`
	CandidateID        int64 `json:"candidate_id,omitzero"`
}

type IdentityCandidateEvidence struct {
	ID                 int64              `json:"id"`
	State              IdentityMatchState `json:"state"`
	ApplicationPending bool               `json:"application_pending"`
	Fingerprint        string             `json:"fingerprint"`
}
type IdentitySourceEvidence struct {
	ID                   int64  `json:"id"`
	Type                 string `json:"type"`
	Identifier           string `json:"identifier"`
	OwnershipFingerprint string `json:"ownership_fingerprint"`
}

// IdentitySourceContribution records native occurrence/observation ownership
// per participant. A union of sources cannot authorize an unscoped endpoint.
type IdentitySourceContribution struct {
	ParticipantID int64 `json:"participant_id"`
	SourceID      int64 `json:"source_id"`
}
type IdentityAccountEvidence struct {
	ID                   int64  `json:"id"`
	ConnectionGeneration int64  `json:"connection_generation"`
	DiscoveryRevision    int64  `json:"discovery_revision"`
	OwnershipFingerprint string `json:"ownership_fingerprint"`
}
type IdentityBookEvidence struct {
	ID           int64  `json:"id"`
	AccountID    int64  `json:"account_id"`
	SyncRevision int64  `json:"sync_revision"`
	CanonicalURL string `json:"canonical_url"`
}
type IdentityResourceEvidence struct {
	ID              int64  `json:"id"`
	PersonID        int64  `json:"person_id"`
	BookID          int64  `json:"book_id"`
	MappingRevision int64  `json:"mapping_revision"`
	Href            string `json:"href"`
	ETag            string `json:"etag"`
	Governance      string `json:"governance"`
}
type IdentityPublicationEvidence struct {
	PersonID         int64  `json:"person_id"`
	BookID           int64  `json:"book_id"`
	MutationRevision int64  `json:"mutation_revision"`
	Desired          bool   `json:"desired"`
	PendingOperation string `json:"pending_operation"`
	Href             string `json:"href"`
}

// IdentityConflictEvidence retains unresolved remote conflict revisions without
// exposing either local or remote contact bodies.
type IdentityConflictEvidence struct {
	ID              int64 `json:"id"`
	PersonID        int64 `json:"person_id"`
	BookID          int64 `json:"book_id"`
	MappingRevision int64 `json:"mapping_revision"`
	ReviewRevision  int64 `json:"review_revision"`
}

type IdentityLineageEvidence struct {
	MergeID         int64  `json:"merge_id"`
	ParticipantID   int64  `json:"participant_id"`
	OriginSide      string `json:"origin_side"`
	CurrentPersonID int64  `json:"current_person_id"`
}

// IdentityOperationPreviewContext never reserves a writer, seeds metadata,
// projects a profile, or promotes a participant to a person.
func (s *Store) IdentityOperationPreviewContext(ctx context.Context, operation identitycontrol.Operation, target identitycontrol.IdentityTarget) (*IdentitySnapshot, error) {
	request := identitycontrol.PreviewRequest{Operation: operation, Target: target}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	var result *IdentitySnapshot
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		result, err = s.identityOperationSnapshotTx(ctx, tx, operation, target.Canonical(operation))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("preview identity operation: %w", err)
	}
	return result, nil
}

func (s *Store) identityOperationSnapshotTx(ctx context.Context, tx *loggedTx, operation identitycontrol.Operation, target identitycontrol.IdentityTarget) (*IdentitySnapshot, error) {
	x := &IdentitySnapshot{Operation: operation, Target: target}
	var err error
	x.IdentityRevision, err = s.currentIdentityRevisionTxContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	x.ComponentMembers, err = identityComponentTx(ctx, tx, target.ParticipantID)
	if err != nil {
		return nil, err
	}
	x.Members = slices.Clone(x.ComponentMembers)
	if target.OtherParticipantID != 0 {
		x.OtherComponentMembers, err = identityComponentTx(ctx, tx, target.OtherParticipantID)
		if err != nil {
			return nil, err
		}
		x.Members = append(x.Members, x.OtherComponentMembers...)
	}
	x.Members = canonicalIdentityIDs(x.Members)
	if len(x.Members) > maxIdentityOperationMembers {
		return nil, ErrIdentityOperationTooLarge
	}
	people, err := personIDsForParticipantsTx(ctx, tx, x.Members)
	if err != nil {
		return nil, err
	}
	if target.PersonID != 0 {
		people = append(people, target.PersonID)
	}
	people = canonicalIdentityIDs(people)
	for _, id := range people {
		var p IdentityPersonEvidence
		if err := tx.QueryRowContext(ctx, `SELECT id,vcard_uid,revision,vcard_projection_revision FROM persons WHERE id=?`, id).Scan(&p.ID, &p.UID, &p.Revision, &p.ProjectionRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrPersonNotFound
			}
			return nil, err
		}
		x.Persons = append(x.Persons, p)
	}
	if len(people) > 0 {
		ph, args := sortedIDPlaceholders(people)
		err = identityReadRows(ctx, tx, `SELECT participant_id,person_id FROM person_participants WHERE person_id IN (`+ph+`) ORDER BY participant_id LIMIT 101`, args, func(rows *loggedRows) error {
			var b IdentityBindingEvidence
			if err := rows.Scan(&b.ParticipantID, &b.PersonID); err != nil {
				return err
			}
			x.Bindings = append(x.Bindings, b)
			x.Members = append(x.Members, b.ParticipantID)
			return nil
		})
		if err != nil {
			return nil, err
		}
		x.Members = canonicalIdentityIDs(x.Members)
		if len(x.Members) > maxIdentityOperationMembers {
			return nil, ErrIdentityOperationTooLarge
		}
	}
	ph, args := sortedIDPlaceholders(x.Members)
	var members []ParticipantIdentityMember
	if err := readParticipantIdentityMembersWith(ctx, tx, &members, ph, args); err != nil {
		return nil, err
	}
	var identifiers []ParticipantIdentifierContext
	if err := readParticipantIdentifierContextWith(ctx, tx, &identifiers, ph, args); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, participantObservationSelect+` WHERE o.participant_id IN (`+ph+`) ORDER BY o.participant_id,o.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read identity contact observations: %w", err)
	}
	observations, err := scanParticipantContactObservations(rows)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		var selected []ParticipantIdentifierContext
		for _, identifier := range identifiers {
			if identifier.ParticipantID == m.ParticipantID {
				selected = append(selected, identifier)
			}
		}
		hash, err := identityEvidenceHash(selected)
		if err != nil {
			return nil, err
		}
		var selectedObservations []ParticipantContactObservation
		for _, observation := range observations {
			if observation.ParticipantID == m.ParticipantID {
				selectedObservations = append(selectedObservations, observation)
			}
		}
		observationHash, err := identityEvidenceHash(selectedObservations)
		if err != nil {
			return nil, err
		}
		x.Participants = append(x.Participants, IdentityParticipantEvidence{ID: m.ParticipantID, DisplayName: m.DisplayName, Email: m.Email, Phone: m.Phone, IdentifierFingerprint: hash, ObservationFingerprint: observationHash})
	}
	linkArgs := append(slices.Clone(args), args...)
	err = identityReadRows(ctx, tx, `SELECT participant_a,participant_b,COALESCE(identity_match_candidate_id,0) FROM participant_links WHERE participant_a IN (`+ph+`) AND participant_b IN (`+ph+`) ORDER BY participant_a,participant_b`, linkArgs, func(rows *loggedRows) error {
		var e IdentityLinkEvidence
		if err := rows.Scan(&e.ParticipantID, &e.OtherParticipantID, &e.CandidateID); err != nil {
			return err
		}
		x.Links = append(x.Links, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.identitySnapshotCandidatesTx(ctx, tx, x); err != nil {
		return nil, err
	}
	if err = s.identitySnapshotSourcesTx(ctx, tx, x); err != nil {
		return nil, err
	}
	if len(people) > 0 {
		if err = s.identitySnapshotPersonsTx(ctx, tx, x, people); err != nil {
			return nil, err
		}
	}
	identitySnapshotEffect(x)
	x.Blockers = slices.Compact(slices.Sorted(slices.Values(x.Blockers)))
	if err := identitySnapshotValidateIDs(x); err != nil {
		return nil, err
	}
	x.Fingerprint, err = identityEvidenceHash(x)
	return x, err
}

func (s *Store) identitySnapshotCandidatesTx(ctx context.Context, tx *loggedTx, x *IdentitySnapshot) error {
	ph, args := sortedIDPlaceholders(x.Members)
	allArgs := append(slices.Clone(args), args...)
	where := `(c.left_kind='participant' AND c.left_id IN (` + ph + `)) OR (c.right_kind='participant' AND c.right_id IN (` + ph + `))`
	if len(x.Persons) > 0 {
		ids := make([]int64, len(x.Persons))
		for i, person := range x.Persons {
			ids[i] = person.ID
		}
		pph, pargs := sortedIDPlaceholders(ids)
		where += ` OR (c.left_kind='person' AND c.left_id IN (` + pph + `)) OR (c.right_kind='person' AND c.right_id IN (` + pph + `))`
		allArgs = append(allArgs, pargs...)
		allArgs = append(allArgs, pargs...)
	}
	var candidates []IdentityMatchCandidate
	if err := identityReadRows(ctx, tx, identityMatchCandidateSelect+` WHERE `+where+` ORDER BY c.id`, allArgs, func(rows *loggedRows) error {
		c, err := scanIdentityMatchCandidate(rows)
		if err != nil {
			return err
		}
		candidates = append(candidates, *c)
		return nil
	}); err != nil {
		return err
	}
	// Reuse native evidence parsers within the same transaction. Finish the
	// candidate cursor first so PostgreSQL never has overlapping result sets.
	for start := 0; start < len(candidates); start += 300 {
		end := min(start+300, len(candidates))
		if err := loadIdentityMatchEvidencePageContext(ctx, tx, candidates[start:end]); err != nil {
			return err
		}
	}
	for i := range candidates {
		c := &candidates[i]
		if err := loadIdentityMatchSourceSupportContext(ctx, tx, c); err != nil {
			return err
		}
		if !identitycontrol.ValidID(c.LeftID) || !identitycontrol.ValidID(c.RightID) {
			return fmt.Errorf("%w: unsafe candidate endpoint", identitycontrol.ErrInvalidRequest)
		}
		for _, support := range c.SourceSupport {
			if !identitycontrol.ValidID(support.SourceID) {
				return fmt.Errorf("%w: unsafe candidate source", identitycontrol.ErrInvalidRequest)
			}
		}
		for _, evidence := range c.Evidence {
			if !identitycontrol.ValidID(evidence.ID) || !identitycontrol.ValidID(evidence.CandidateID) {
				return fmt.Errorf("%w: unsafe candidate evidence", identitycontrol.ErrInvalidRequest)
			}
			for _, support := range evidence.SourceSupport {
				if !identitycontrol.ValidID(support.SourceID) {
					return fmt.Errorf("%w: unsafe evidence source", identitycontrol.ErrInvalidRequest)
				}
			}
		}
		// UpdatedAt is a bookkeeping clock, not semantic identity evidence.
		c.UpdatedAt = c.CreatedAt
		hash, err := identityEvidenceHash(struct {
			Candidate                 *IdentityMatchCandidate
			ObservationConflictOrigin sql.NullString
			PreConflictState          sql.NullString
		}{c, c.conflictState.observationOrigin, c.conflictState.preConflictState})
		if err != nil {
			return err
		}
		x.Candidates = append(x.Candidates, IdentityCandidateEvidence{ID: c.ID, State: c.State, ApplicationPending: c.ApplicationPending, Fingerprint: hash})
		if c.State == IdentityMatchStateConflict && (x.Operation == identitycontrol.OperationPersonLink || x.Operation == identitycontrol.OperationPersonUnlink) {
			x.Blockers = append(x.Blockers, "conflicting-identity-evidence")
		}
	}
	return nil
}

func canonicalIdentityIDs(ids []int64) []int64 {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func identityReadRows(ctx context.Context, tx *loggedTx, query string, args []any, read func(*loggedRows) error) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := read(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func identityComponentTx(ctx context.Context, tx *loggedTx, id int64) ([]int64, error) {
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participants WHERE id=?)`, id).Scan(&found); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrParticipantNotFound
	}
	// No ORDER BY before LIMIT: both engines can stop recursive discovery once
	// the complete-scope contract is known to be exceeded. UNION also handles
	// legacy cycles without visiting a participant repeatedly.
	var ids []int64
	err := identityReadRows(ctx, tx, `WITH RECURSIVE component(id) AS (
		SELECT CAST(? AS BIGINT) UNION
		SELECT CASE WHEN l.participant_a=c.id THEN l.participant_b ELSE l.participant_a END
		FROM participant_links l JOIN component c ON l.participant_a=c.id OR l.participant_b=c.id
	) SELECT id FROM component LIMIT 101`, []any{id}, func(rows *loggedRows) error {
		var member int64
		if err := rows.Scan(&member); err != nil {
			return err
		}
		ids = append(ids, member)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(ids) > maxIdentityOperationMembers {
		return nil, ErrIdentityOperationTooLarge
	}
	return canonicalIdentityIDs(ids), nil
}

func identityEvidenceHash(value any) (string, error) {
	b, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *Store) identitySnapshotSourcesTx(ctx context.Context, tx *loggedTx, x *IdentitySnapshot) error {
	ph, args := sortedIDPlaceholders(x.Members)
	allArgs := append(slices.Clone(args), args...)
	allArgs = append(allArgs, args...)
	// Hidden and source-deleted occurrences remain archive ownership evidence.
	// Neither identifier strings nor source_ref text can assign an account.
	var sourceIDs []int64
	if err := identityReadRows(ctx, tx, `SELECT sender_id AS participant_id,source_id FROM messages WHERE sender_id IN (`+ph+`)
		UNION SELECT r.participant_id,m.source_id FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE r.participant_id IN (`+ph+`)
		UNION SELECT participant_id,source_id FROM participant_contact_observations WHERE participant_id IN (`+ph+`) AND source_id IS NOT NULL
		ORDER BY participant_id,source_id`, allArgs, func(rows *loggedRows) error {
		var c IdentitySourceContribution
		if err := rows.Scan(&c.ParticipantID, &c.SourceID); err != nil {
			return err
		}
		x.SourceContributions = append(x.SourceContributions, c)
		sourceIDs = append(sourceIDs, c.SourceID)
		return nil
	}); err != nil {
		return err
	}
	if len(sourceIDs) == 0 {
		return nil
	}
	sph, sargs := sortedIDPlaceholders(sourceIDs)
	return identityReadRows(ctx, tx, `SELECT id,source_type,identifier,COALESCE(google_user_id,''),COALESCE(CAST(sync_config AS TEXT),''),COALESCE(oauth_app,'') FROM sources WHERE id IN (`+sph+`) ORDER BY id`, sargs, func(rows *loggedRows) error {
		var source IdentitySourceEvidence
		var owner, config, app string
		if err := rows.Scan(&source.ID, &source.Type, &source.Identifier, &owner, &config, &app); err != nil {
			return err
		}
		var err error
		source.OwnershipFingerprint, err = identityEvidenceHash([]string{owner, config, app})
		if err != nil {
			return err
		}
		x.Sources = append(x.Sources, source)
		return nil
	})
}

func (s *Store) identitySnapshotPersonsTx(ctx context.Context, tx *loggedTx, x *IdentitySnapshot, people []int64) error {
	ph, args := sortedIDPlaceholders(people)
	if err := identityReadRows(ctx, tx, `SELECT id FROM person_merges WHERE current_person_id IN (`+ph+`) ORDER BY id`, args, func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		x.ActiveMergeIDs = append(x.ActiveMergeIDs, id)
		return nil
	}); err != nil {
		return err
	}
	mph, margs := sortedIDPlaceholders(x.Members)
	if err := identityReadRows(ctx, tx, `SELECT l.merge_id,l.participant_id,l.origin_side,m.current_person_id FROM person_merge_participants l JOIN person_merges m ON m.id=l.merge_id WHERE l.participant_id IN (`+mph+`) AND l.split_id IS NULL AND m.current_person_id IS NOT NULL ORDER BY l.merge_id,l.participant_id`, margs, func(rows *loggedRows) error {
		var l IdentityLineageEvidence
		if err := rows.Scan(&l.MergeID, &l.ParticipantID, &l.OriginSide, &l.CurrentPersonID); err != nil {
			return err
		}
		x.Lineages = append(x.Lineages, l)
		return nil
	}); err != nil {
		return err
	}
	if err := identityReadRows(ctx, tx, `SELECT id,person_id,address_book_id,mapping_revision,href,remote_etag,governance FROM carddav_resources WHERE person_id IN (`+ph+`) ORDER BY id`, args, func(rows *loggedRows) error {
		var r IdentityResourceEvidence
		if err := rows.Scan(&r.ID, &r.PersonID, &r.BookID, &r.MappingRevision, &r.Href, &r.ETag, &r.Governance); err != nil {
			return err
		}
		x.Resources = append(x.Resources, r)
		return nil
	}); err != nil {
		return err
	}
	if err := identityReadRows(ctx, tx, `SELECT person_id,COALESCE(address_book_id,0),mutation_revision,desired,COALESCE(pending_operation,''),COALESCE(href,'') FROM carddav_publications WHERE person_id IN (`+ph+`) ORDER BY person_id`, args, func(rows *loggedRows) error {
		var p IdentityPublicationEvidence
		if err := rows.Scan(&p.PersonID, &p.BookID, &p.MutationRevision, &p.Desired, &p.PendingOperation, &p.Href); err != nil {
			return err
		}
		x.Publications = append(x.Publications, p)
		return nil
	}); err != nil {
		return err
	}
	if err := identityReadRows(ctx, tx, `SELECT c.id,r.person_id,c.address_book_id,c.mapping_revision,c.review_revision
 FROM carddav_conflicts c JOIN carddav_resources r ON r.address_book_id=c.address_book_id AND r.href=c.href
 WHERE r.person_id IN (`+ph+`) AND c.status='unresolved' ORDER BY c.id`, args, func(rows *loggedRows) error {
		var c IdentityConflictEvidence
		if err := rows.Scan(&c.ID, &c.PersonID, &c.BookID, &c.MappingRevision, &c.ReviewRevision); err != nil {
			return err
		}
		x.Conflicts = append(x.Conflicts, c)
		return nil
	}); err != nil {
		return err
	}
	var books []int64
	for _, r := range x.Resources {
		books = append(books, r.BookID)
	}
	for _, p := range x.Publications {
		if p.BookID > 0 {
			books = append(books, p.BookID)
		}
	}
	books = canonicalIdentityIDs(books)
	if len(books) > 0 {
		bph, bargs := sortedIDPlaceholders(books)
		var accounts []int64
		if err := identityReadRows(ctx, tx, `SELECT id,account_id,sync_revision,canonical_url FROM carddav_address_books WHERE id IN (`+bph+`) ORDER BY id`, bargs, func(rows *loggedRows) error {
			var b IdentityBookEvidence
			if err := rows.Scan(&b.ID, &b.AccountID, &b.SyncRevision, &b.CanonicalURL); err != nil {
				return err
			}
			x.Books = append(x.Books, b)
			accounts = append(accounts, b.AccountID)
			return nil
		}); err != nil {
			return err
		}
		aph, aargs := sortedIDPlaceholders(accounts)
		if err := identityReadRows(ctx, tx, `SELECT id,connection_generation,discovery_revision,connection_name,base_url,username,principal_url,home_url FROM carddav_accounts WHERE id IN (`+aph+`) ORDER BY id`, aargs, func(rows *loggedRows) error {
			var a IdentityAccountEvidence
			var name, base, user, principal, home string
			if err := rows.Scan(&a.ID, &a.ConnectionGeneration, &a.DiscoveryRevision, &name, &base, &user, &principal, &home); err != nil {
				return err
			}
			var err error
			a.OwnershipFingerprint, err = identityEvidenceHash([]string{name, base, user, principal, home})
			if err != nil {
				return err
			}
			x.Accounts = append(x.Accounts, a)
			return nil
		}); err != nil {
			return err
		}
	}
	if x.Operation == identitycontrol.OperationPersonLink || x.Operation == identitycontrol.OperationPersonUnlink {
		if len(x.Conflicts) > 0 {
			x.Blockers = append(x.Blockers, "carddav-publication")
		}
		if len(x.ActiveMergeIDs) > 0 || len(x.Lineages) > 0 {
			x.Blockers = append(x.Blockers, "active-merge-lineage")
		}
		for _, id := range people {
			err := ensurePersonMergeCardDAVStateTx(ctx, tx, id, id)
			if errors.Is(err, ErrPersonCardDAVPublished) {
				x.Blockers = append(x.Blockers, "carddav-publication")
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}

func identitySnapshotEffect(x *IdentitySnapshot) {
	graph := x.Operation == identitycontrol.OperationGraphLink || x.Operation == identitycontrol.OperationGraphUnlink
	if graph {
		var edge *IdentityLinkEvidence
		for i := range x.Links {
			e := &x.Links[i]
			if e.ParticipantID == x.Target.ParticipantID && e.OtherParticipantID == x.Target.OtherParticipantID {
				edge = e
				break
			}
		}
		if x.Operation == identitycontrol.OperationGraphUnlink {
			x.Noop = edge == nil
			return
		}
		if edge != nil {
			x.ManualConfirmCandidateID = edge.CandidateID
			x.Noop = edge.CandidateID == 0
			return
		}
		if slices.Contains(x.ComponentMembers, x.Target.OtherParticipantID) {
			x.Blockers = append(x.Blockers, "already-connected")
		}
		var people []int64
		for _, b := range x.Bindings {
			if slices.Contains(x.ComponentMembers, b.ParticipantID) || slices.Contains(x.OtherComponentMembers, b.ParticipantID) {
				people = append(people, b.PersonID)
			}
		}
		if len(canonicalIdentityIDs(people)) > 1 {
			x.Blockers = append(x.Blockers, "person-merge-required")
		}
		return
	}
	bound := 0
	for _, b := range x.Bindings {
		if !slices.Contains(x.ComponentMembers, b.ParticipantID) {
			continue
		}
		if b.PersonID != x.Target.PersonID {
			x.Blockers = append(x.Blockers, "person-merge-required")
		} else {
			bound++
		}
	}
	if x.Operation == identitycontrol.OperationPersonLink {
		x.Noop = bound == len(x.ComponentMembers)
	} else {
		x.Noop = bound == 0
	}
}

// Validate every derived identifier before returning evidence over JSON. Zero
// is allowed only for fields whose native schema explicitly permits absence.
func identitySnapshotValidateIDs(x *IdentitySnapshot) error {
	ids := slices.Clone(x.Members)
	ids = append(ids, x.ComponentMembers...)
	ids = append(ids, x.OtherComponentMembers...)
	ids = append(ids, x.ActiveMergeIDs...)
	for _, p := range x.Participants {
		ids = append(ids, p.ID)
	}
	for _, p := range x.Persons {
		ids = append(ids, p.ID)
	}
	for _, b := range x.Bindings {
		ids = append(ids, b.ParticipantID, b.PersonID)
	}
	for _, l := range x.Links {
		ids = append(ids, l.ParticipantID, l.OtherParticipantID)
		if l.CandidateID != 0 {
			ids = append(ids, l.CandidateID)
		}
	}
	for _, c := range x.Candidates {
		ids = append(ids, c.ID)
	}
	for _, s := range x.Sources {
		ids = append(ids, s.ID)
	}
	for _, s := range x.SourceContributions {
		ids = append(ids, s.ParticipantID, s.SourceID)
	}
	for _, a := range x.Accounts {
		ids = append(ids, a.ID)
	}
	for _, b := range x.Books {
		ids = append(ids, b.ID, b.AccountID)
	}
	for _, r := range x.Resources {
		ids = append(ids, r.ID, r.PersonID, r.BookID)
	}
	for _, p := range x.Publications {
		ids = append(ids, p.PersonID)
		if p.BookID != 0 {
			ids = append(ids, p.BookID)
		}
	}
	for _, c := range x.Conflicts {
		ids = append(ids, c.ID, c.PersonID, c.BookID)
	}
	for _, l := range x.Lineages {
		ids = append(ids, l.MergeID, l.ParticipantID, l.CurrentPersonID)
	}
	if x.ManualConfirmCandidateID != 0 {
		ids = append(ids, x.ManualConfirmCandidateID)
	}
	for _, id := range ids {
		if !identitycontrol.ValidID(id) {
			return fmt.Errorf("%w: derived identity ID must be an exact positive JSON integer", identitycontrol.ErrInvalidRequest)
		}
	}
	return nil
}
