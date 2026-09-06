package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	minPersonNetworkDepth = 1
	maxPersonNetworkDepth = 3
	maxPersonNetworkNodes = 250
	maxPersonNetworkEdges = 500
)

var ErrPersonNetworkInvalid = errors.New("invalid person network")

// PersonNetworkOptions bounds the curated relationship graph around one
// durable person root.
type PersonNetworkOptions struct {
	Depth        int
	IncludeEnded bool
}

// PersonNetwork is a bounded projection over declared person relationships
// and employment records. It never includes archive-derived associations.
type PersonNetwork struct {
	RootPersonID int64         `json:"root_person_id"`
	Depth        int           `json:"depth"`
	Truncated    bool          `json:"truncated"`
	Nodes        []NetworkNode `json:"nodes"`
	Edges        []NetworkEdge `json:"edges"`
}

// NetworkNode identifies a curated person or organization in a person
// network. ID is globally typed so person and organization IDs cannot collide.
type NetworkNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" enum:"person,organization"`
	EntityID int64  `json:"entity_id"`
	Label    string `json:"label"`
	Hop      int    `json:"hop"`
}

// NetworkEdge identifies a declared relationship or employment connection.
type NetworkEdge struct {
	ID                   string  `json:"id"`
	Kind                 string  `json:"kind" enum:"relationship,employment"`
	SourceNodeID         string  `json:"source_node_id"`
	TargetNodeID         string  `json:"target_node_id"`
	RelationshipTypeSlug *string `json:"relationship_type_slug,omitempty"`
	Label                string  `json:"label"`
	StartDate            *string `json:"start_date,omitempty"`
	EndDate              *string `json:"end_date,omitempty"`
}

// GetPersonNetworkContext returns a deterministic, breadth-first projection
// of one person's declared network. Only person_relationships and employments
// can introduce an edge; archive observations are intentionally excluded.
func (s *Store) GetPersonNetworkContext(
	ctx context.Context, personID int64, opts PersonNetworkOptions,
) (PersonNetwork, error) {
	if opts.Depth < minPersonNetworkDepth || opts.Depth > maxPersonNetworkDepth {
		return PersonNetwork{}, fmt.Errorf("%w: depth must be between %d and %d",
			ErrPersonNetworkInvalid, minPersonNetworkDepth, maxPersonNetworkDepth)
	}
	root, err := s.personNetworkPersonNode(ctx, personID, 0)
	if err != nil {
		return PersonNetwork{}, err
	}

	traversal := personNetworkTraversal{
		store: s,
		ctx:   ctx,
		opts:  opts,
		nodes: map[string]NetworkNode{root.ID: root},
		edges: make(map[string]NetworkEdge),
		seen:  make(map[string][]int64),
	}
	frontier := []NetworkNode{root}
	for hop := 0; hop < opts.Depth && len(frontier) > 0; hop++ {
		candidates, readErr := traversal.readLayer(frontier, hop+1)
		if readErr != nil {
			return PersonNetwork{}, readErr
		}
		frontier = traversal.admit(candidates)
		if traversal.truncated {
			break
		}
	}

	graph := PersonNetwork{
		RootPersonID: personID,
		Depth:        opts.Depth,
		Truncated:    traversal.truncated,
		Nodes:        make([]NetworkNode, 0, len(traversal.nodes)),
		Edges:        make([]NetworkEdge, 0, len(traversal.edges)),
	}
	for _, node := range traversal.nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	for _, edge := range traversal.edges {
		graph.Edges = append(graph.Edges, edge)
	}
	sortNetworkNodes(graph.Nodes)
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].Kind != graph.Edges[j].Kind {
			return graph.Edges[i].Kind < graph.Edges[j].Kind
		}
		if graph.Edges[i].Label != graph.Edges[j].Label {
			return graph.Edges[i].Label < graph.Edges[j].Label
		}
		return personNetworkIDLess(graph.Edges[i].ID, graph.Edges[j].ID)
	})
	return graph, nil
}

type personNetworkTraversal struct {
	store     *Store
	ctx       context.Context
	opts      PersonNetworkOptions
	nodes     map[string]NetworkNode
	edges     map[string]NetworkEdge
	seen      map[string][]int64
	truncated bool
}

type personNetworkCandidate struct {
	node   NetworkNode
	edge   NetworkEdge
	source personNetworkSourceEdge
}

// personNetworkSourceEdge is one distinct edge adjacent to the current
// frontier that no earlier hop admitted, together with the node it reaches.
type personNetworkSourceEdge struct {
	edgeKind     string
	edgeEntityID int64
	nodeKind     string
	nodeEntityID int64
}

type personNetworkHydratedEdge struct {
	edge       NetworkEdge
	sourceNode NetworkNode
	targetNode NetworkNode
}

// readLayer reads the next hop's candidates in public order. The edge budget
// is charged only against distinct edges that no earlier hop admitted, so an
// edge read from both of its frontier endpoints, or already present in the
// graph, costs nothing.
func (t *personNetworkTraversal) readLayer(
	frontier []NetworkNode, hop int,
) ([]personNetworkCandidate, error) {
	remaining := maxPersonNetworkEdges - len(t.edges)
	limit := remaining + 1
	sources, err := t.store.readPersonNetworkLayerSources(
		t.ctx, frontier, t.seen, t.opts.IncludeEnded, limit)
	if err != nil {
		return nil, err
	}
	if hook := t.store.personNetworkSourceReadHook; hook != nil {
		hook(limit, len(sources))
	}
	if len(sources) > remaining {
		t.truncated = true
		sources = sources[:remaining]
	}
	return t.store.hydratePersonNetworkLayerCandidates(t.ctx, sources, hop)
}

// admit consumes the public-order prefix of the bounded layer. Stopping at
// the first node or edge omission keeps admission deterministic even when the
// edge budget underfills the node cap.
func (t *personNetworkTraversal) admit(candidates []personNetworkCandidate) []NetworkNode {
	next := make([]NetworkNode, 0)
	for _, candidate := range candidates {
		_, nodeExists := t.nodes[candidate.node.ID]
		if !nodeExists && len(t.nodes) >= maxPersonNetworkNodes {
			t.truncated = true
			break
		}
		if len(t.edges) >= maxPersonNetworkEdges {
			t.truncated = true
			break
		}
		t.edges[candidate.edge.ID] = candidate.edge
		t.seen[candidate.source.edgeKind] = append(
			t.seen[candidate.source.edgeKind], candidate.source.edgeEntityID)
		if nodeExists {
			continue
		}
		t.nodes[candidate.node.ID] = candidate.node
		next = append(next, candidate.node)
	}
	return next
}

func (s *Store) readPersonNetworkLayerSources(
	ctx context.Context, frontier []NetworkNode, seen map[string][]int64,
	includeEnded bool, limit int,
) ([]personNetworkSourceEdge, error) {
	people := make([]int64, 0, len(frontier))
	organizations := make([]int64, 0)
	for _, node := range frontier {
		switch node.Kind {
		case "person":
			people = append(people, node.EntityID)
		case "organization":
			organizations = append(organizations, node.EntityID)
		default:
			return nil, fmt.Errorf("%w: unknown node kind %q", ErrPersonNetworkInvalid, node.Kind)
		}
	}
	query, args := s.personNetworkLayerSourcesQuery(people, organizations, seen, includeEnded, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read person network layer: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sources := make([]personNetworkSourceEdge, 0, limit)
	for rows.Next() {
		var source personNetworkSourceEdge
		if err := rows.Scan(&source.edgeKind, &source.edgeEntityID, &source.nodeKind, &source.nodeEntityID); err != nil {
			return nil, fmt.Errorf("scan person network layer: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read person network layer: %w", err)
	}
	return sources, nil
}

// personNetworkLayerSourcesQuery selects every current edge adjacent to the
// frontier that is not already in the graph, keeps one row per edge (the
// endpoint that sorts first), and returns the first limit rows in the same
// order the public response uses: node kind, node label, node ID, edge kind,
// edge ID. Labels sort bytewise so SQL and Go agree on the prefix.
func (s *Store) personNetworkLayerSourcesQuery(
	people, organizations []int64, seen map[string][]int64, includeEnded bool, limit int,
) (string, []any) {
	collation := s.bytewiseTextCollation()
	ctes := make([]string, 0, 4)
	args := make([]any, 0)
	addCTE := func(name string, ids []int64) {
		clause, values := personNetworkIDValuesCTE(name, ids)
		ctes = append(ctes, clause)
		args = append(args, values...)
	}
	relationshipFilter, employmentFilter := "TRUE", "TRUE"
	if !includeEnded {
		relationshipFilter = "relationship.end_year IS NULL"
		employmentFilter = s.dialect.BoolTrueExpr("employment.is_current")
	}
	if ids := seen["relationship"]; len(ids) > 0 {
		addCTE("seen_relationships", ids)
		relationshipFilter += ` AND NOT EXISTS (SELECT 1 FROM seen_relationships seen WHERE seen.id = relationship.id)`
	}
	if ids := seen["employment"]; len(ids) > 0 {
		addCTE("seen_employments", ids)
		employmentFilter += ` AND NOT EXISTS (SELECT 1 FROM seen_employments seen WHERE seen.id = employment.id)`
	}
	personLabel := func(alias string) string {
		return `COALESCE(NULLIF(` + alias + `.display_name, ''), ` + alias + `.vcard_uid)`
	}
	// CROSS JOIN pins the frontier as the outer loop on SQLite, so each
	// frontier node probes its adjacency index instead of the planner
	// scanning every edge and filtering by the small frontier set.
	// PostgreSQL treats CROSS JOIN plus WHERE as an ordinary inner join.
	branches := make([]string, 0, 4)
	if len(people) > 0 {
		addCTE("frontier_people", people)
		branches = append(branches,
			`SELECT 'relationship', relationship.id, 'person', relationship.target_person_id,
			        `+personLabel("target_person")+`
			 FROM frontier_people frontier
			 CROSS JOIN person_relationships relationship
			 JOIN persons target_person ON target_person.id = relationship.target_person_id
			 WHERE relationship.source_person_id = frontier.id AND `+relationshipFilter,
			`SELECT 'relationship', relationship.id, 'person', relationship.source_person_id,
			        `+personLabel("source_person")+`
			 FROM frontier_people frontier
			 CROSS JOIN person_relationships relationship
			 JOIN persons source_person ON source_person.id = relationship.source_person_id
			 WHERE relationship.target_person_id = frontier.id AND `+relationshipFilter,
			`SELECT 'employment', employment.id, 'organization', employment.organization_id, organization.name
			 FROM frontier_people frontier
			 CROSS JOIN employments employment
			 JOIN organizations organization ON organization.id = employment.organization_id
			 WHERE employment.person_id = frontier.id AND `+employmentFilter,
		)
	}
	if len(organizations) > 0 {
		addCTE("frontier_organizations", organizations)
		branches = append(branches,
			`SELECT 'employment', employment.id, 'person', employment.person_id, `+personLabel("person")+`
			 FROM frontier_organizations frontier
			 CROSS JOIN employments employment
			 JOIN persons person ON person.id = employment.person_id
			 WHERE employment.organization_id = frontier.id AND `+employmentFilter,
		)
	}
	nodeOrder := `node_kind` + collation + `, node_label` + collation + `, node_id`
	query := `WITH ` + strings.Join(ctes, ",\n") + `,
		layer(edge_kind, edge_id, node_kind, node_id, node_label) AS (` + strings.Join(branches, "\nUNION ALL\n") + `),
		ranked AS (
			SELECT edge_kind, edge_id, node_kind, node_id, node_label,
			       ROW_NUMBER() OVER (PARTITION BY edge_kind, edge_id ORDER BY ` + nodeOrder + `) AS candidate_rank
			FROM layer
		)
		SELECT edge_kind, edge_id, node_kind, node_id
		FROM ranked
		WHERE candidate_rank = 1
		ORDER BY ` + nodeOrder + `, edge_kind` + collation + `, edge_id
		LIMIT ?`
	return query, append(args, limit)
}

func (s *Store) hydratePersonNetworkLayerCandidates(
	ctx context.Context, sources []personNetworkSourceEdge, hop int,
) ([]personNetworkCandidate, error) {
	ids := make(map[string][]int64, 2)
	for _, source := range sources {
		switch source.edgeKind {
		case "relationship", "employment":
			ids[source.edgeKind] = append(ids[source.edgeKind], source.edgeEntityID)
		default:
			return nil, fmt.Errorf("%w: unknown edge kind %q", ErrPersonNetworkInvalid, source.edgeKind)
		}
	}
	hydrated := make(map[string]personNetworkHydratedEdge, len(sources))
	if len(ids["relationship"]) > 0 {
		edges, err := s.hydratePersonNetworkRelationships(ctx, ids["relationship"])
		if err != nil {
			return nil, err
		}
		for id, edge := range edges {
			hydrated[personNetworkEdgeID("relationship", id)] = edge
		}
	}
	if len(ids["employment"]) > 0 {
		edges, err := s.hydratePersonNetworkEmployments(ctx, ids["employment"])
		if err != nil {
			return nil, err
		}
		for id, edge := range edges {
			hydrated[personNetworkEdgeID("employment", id)] = edge
		}
	}
	candidates := make([]personNetworkCandidate, 0, len(sources))
	for _, source := range sources {
		key := personNetworkEdgeID(source.edgeKind, source.edgeEntityID)
		edge, exists := hydrated[key]
		if !exists {
			return nil, fmt.Errorf("hydrate person network edge %s: missing row", key)
		}
		var node NetworkNode
		switch personNetworkNodeID(source.nodeKind, source.nodeEntityID) {
		case edge.sourceNode.ID:
			node = edge.sourceNode
		case edge.targetNode.ID:
			node = edge.targetNode
		default:
			return nil, fmt.Errorf("%w: edge %s does not reach node %s:%d",
				ErrPersonNetworkInvalid, key, source.nodeKind, source.nodeEntityID)
		}
		node.Hop = hop
		candidates = append(candidates, personNetworkCandidate{node: node, edge: edge.edge, source: source})
	}
	return candidates, nil
}

func (s *Store) hydratePersonNetworkRelationships(
	ctx context.Context, ids []int64,
) (map[int64]personNetworkHydratedEdge, error) {
	cte, args := personNetworkIDValuesCTE("selected_relationships", ids)
	rows, err := s.db.QueryContext(ctx, `
		WITH `+cte+`
		SELECT relationship.id,
		       relationship.source_person_id,
		       COALESCE(NULLIF(source_person.display_name, ''), source_person.vcard_uid),
		       relationship.target_person_id,
		       COALESCE(NULLIF(target_person.display_name, ''), target_person.vcard_uid),
		       relationship_type.slug,
		       relationship_type.forward_label,
		       relationship.start_year, relationship.start_month, relationship.start_day,
		       relationship.end_year, relationship.end_month, relationship.end_day
		FROM selected_relationships selected
		JOIN person_relationships relationship ON relationship.id = selected.id
		JOIN relationship_types relationship_type ON relationship_type.id = relationship.relationship_type_id
		JOIN persons source_person ON source_person.id = relationship.source_person_id
		JOIN persons target_person ON target_person.id = relationship.target_person_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrate person network relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hydrated := make(map[int64]personNetworkHydratedEdge, len(ids))
	for rows.Next() {
		var (
			id, sourceID, targetID                int64
			sourceLabel, targetLabel, slug, label string
			startYear, startMonth, startDay       sql.NullInt64
			endYear, endMonth, endDay             sql.NullInt64
		)
		if err := rows.Scan(
			&id, &sourceID, &sourceLabel, &targetID, &targetLabel, &slug, &label,
			&startYear, &startMonth, &startDay, &endYear, &endMonth, &endDay,
		); err != nil {
			return nil, fmt.Errorf("scan person network relationship: %w", err)
		}
		relationshipSlug := slug
		hydrated[id] = personNetworkHydratedEdge{
			edge: NetworkEdge{
				ID:                   personNetworkEdgeID("relationship", id),
				Kind:                 "relationship",
				SourceNodeID:         personNetworkNodeID("person", sourceID),
				TargetNodeID:         personNetworkNodeID("person", targetID),
				RelationshipTypeSlug: &relationshipSlug,
				Label:                label,
				StartDate:            personNetworkDateFromColumns(startYear, startMonth, startDay),
				EndDate:              personNetworkDateFromColumns(endYear, endMonth, endDay),
			},
			sourceNode: NetworkNode{
				ID: personNetworkNodeID("person", sourceID), Kind: "person", EntityID: sourceID, Label: sourceLabel,
			},
			targetNode: NetworkNode{
				ID: personNetworkNodeID("person", targetID), Kind: "person", EntityID: targetID, Label: targetLabel,
			},
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate person network relationships: %w", err)
	}
	return hydrated, nil
}

func (s *Store) hydratePersonNetworkEmployments(
	ctx context.Context, ids []int64,
) (map[int64]personNetworkHydratedEdge, error) {
	cte, args := personNetworkIDValuesCTE("selected_employments", ids)
	rows, err := s.db.QueryContext(ctx, `
		WITH `+cte+`
		SELECT employment.id,
		       employment.person_id,
		       COALESCE(NULLIF(person.display_name, ''), person.vcard_uid),
		       employment.organization_id,
		       organization.name,
		       COALESCE(NULLIF(employment.title, ''), NULLIF(employment.role, ''), 'employment'),
		       employment.start_year, employment.start_month, employment.start_day,
		       employment.end_year, employment.end_month, employment.end_day
		FROM selected_employments selected
		JOIN employments employment ON employment.id = selected.id
		JOIN persons person ON person.id = employment.person_id
		JOIN organizations organization ON organization.id = employment.organization_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrate person network employments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hydrated := make(map[int64]personNetworkHydratedEdge, len(ids))
	for rows.Next() {
		var (
			id, personID, organizationID              int64
			personLabel, organizationLabel, edgeLabel string
			startYear, startMonth, startDay           sql.NullInt64
			endYear, endMonth, endDay                 sql.NullInt64
		)
		if err := rows.Scan(
			&id, &personID, &personLabel, &organizationID, &organizationLabel, &edgeLabel,
			&startYear, &startMonth, &startDay, &endYear, &endMonth, &endDay,
		); err != nil {
			return nil, fmt.Errorf("scan person network employment: %w", err)
		}
		hydrated[id] = personNetworkHydratedEdge{
			edge: NetworkEdge{
				ID:           personNetworkEdgeID("employment", id),
				Kind:         "employment",
				SourceNodeID: personNetworkNodeID("person", personID),
				TargetNodeID: personNetworkNodeID("organization", organizationID),
				Label:        edgeLabel,
				StartDate:    personNetworkDateFromColumns(startYear, startMonth, startDay),
				EndDate:      personNetworkDateFromColumns(endYear, endMonth, endDay),
			},
			sourceNode: NetworkNode{
				ID: personNetworkNodeID("person", personID), Kind: "person", EntityID: personID, Label: personLabel,
			},
			targetNode: NetworkNode{
				ID: personNetworkNodeID("organization", organizationID), Kind: "organization", EntityID: organizationID, Label: organizationLabel,
			},
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate person network employments: %w", err)
	}
	return hydrated, nil
}

func personNetworkEdgeID(kind string, entityID int64) string {
	return fmt.Sprintf("%s:%d", kind, entityID)
}

func personNetworkIDValuesCTE(name string, ids []int64) (string, []any) {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	values := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		values[index] = "(CAST(? AS BIGINT))"
		args[index] = id
	}
	return name + "(id) AS (VALUES " + strings.Join(values, ", ") + ")", args
}

func personNetworkDateFromColumns(year, month, day sql.NullInt64) *string {
	date := ScanPartialDate(year, month, day)
	if date.IsZero() {
		return nil
	}
	return personNetworkDate(&date)
}

func (s *Store) personNetworkPersonNode(ctx context.Context, personID int64, hop int) (NetworkNode, error) {
	var (
		id          int64
		displayName sql.NullString
		vcardUID    string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, vcard_uid
		FROM persons
		WHERE id = ?
	`, personID).Scan(&id, &displayName, &vcardUID)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkNode{}, ErrPersonNotFound
	}
	if err != nil {
		return NetworkNode{}, fmt.Errorf("get person network node %d: %w", personID, err)
	}
	label := vcardUID
	if displayName.Valid && displayName.String != "" {
		label = displayName.String
	}
	return NetworkNode{ID: personNetworkNodeID("person", id), Kind: "person", EntityID: id, Label: label, Hop: hop}, nil
}

func personNetworkNodeID(kind string, entityID int64) string {
	return fmt.Sprintf("%s:%d", kind, entityID)
}

func personNetworkDate(date *PartialDate) *string {
	if date == nil {
		return nil
	}
	value := date.String()
	return &value
}

func sortNetworkNodes(nodes []NetworkNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hop != nodes[j].Hop {
			return nodes[i].Hop < nodes[j].Hop
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		if nodes[i].Label != nodes[j].Label {
			return nodes[i].Label < nodes[j].Label
		}
		return nodes[i].EntityID < nodes[j].EntityID
	})
}

// personNetworkIDLess orders two IDs of the same kind by their numeric entity
// ID. Both share the "kind:" prefix and carry a decimal without leading
// zeros, so a shorter ID is smaller and equal lengths compare digit by digit.
func personNetworkIDLess(left, right string) bool {
	if len(left) != len(right) {
		return len(left) < len(right)
	}
	return left < right
}
