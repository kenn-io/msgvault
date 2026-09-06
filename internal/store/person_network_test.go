package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type networkSourceRead struct {
	limit int
	count int
}

func TestGetPersonNetworkContextUsesCuratedEdgesOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	s := f.Store
	root := createNetworkPerson(t, s, "Root")
	peer := createNetworkPerson(t, s, "Peer")
	organization := createNetworkOrganization(t, s, "Example Works")
	createNetworkRelationship(t, s, root.ID, peer.ID, "colleague")
	createNetworkEmployment(t, s, peer.ID, organization.ID, true)
	messageOnlyParticipantID, err := s.EnsureParticipant("message-only@example.test", "Message Only", "example.test")
	require.NoError(err)
	messageOnly, _, err := s.CreatePersonFromParticipantContext(t.Context(), messageOnlyParticipantID)
	require.NoError(err)
	messageID := f.CreateMessage("archive-linked-message-only")
	require.NotEmpty(root.ParticipantIDs)
	require.NoError(s.ReplaceMessageRecipients(messageID, "from", []int64{root.ParticipantIDs[0]}, []string{"Root"}))
	require.NoError(s.ReplaceMessageRecipients(messageID, "to", []int64{messageOnlyParticipantID}, []string{"Message Only"}))

	graph, err := s.GetPersonNetworkContext(context.Background(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.Len(graph.Nodes, 3)
	assert.Len(graph.Edges, 2)
	assert.NotContains(networkNodeIDs(graph.Nodes), fmt.Sprintf("person:%d", messageOnly.ID))
}

func TestGetPersonNetworkContextReturnsRootOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 1})
	require.NoError(err)
	require.Len(graph.Nodes, 1)
	assert.Equal(store.NetworkNode{
		ID:       fmt.Sprintf("person:%d", root.ID),
		Kind:     "person",
		EntityID: root.ID,
		Label:    "Root",
		Hop:      0,
	}, graph.Nodes[0])
	assert.Empty(graph.Edges)
}

func TestGetPersonNetworkContextUsesFirstHopAndStableFrontierOrder(t *testing.T) {
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	zulu := createNetworkPerson(t, f.Store, "Zulu")
	alpha := createNetworkPerson(t, f.Store, "Alpha")
	organization := createNetworkOrganization(t, f.Store, "Example Works")
	createNetworkRelationship(t, f.Store, root.ID, zulu.ID, "friend")
	createNetworkRelationship(t, f.Store, root.ID, alpha.ID, "friend")
	createNetworkEmployment(t, f.Store, alpha.ID, organization.ID, true)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(t, err)
	require.Len(t, graph.Nodes, 4)
	assert.Equal(t, []store.NetworkNode{
		{ID: fmt.Sprintf("person:%d", root.ID), Kind: "person", EntityID: root.ID, Label: "Root", Hop: 0},
		{ID: fmt.Sprintf("person:%d", alpha.ID), Kind: "person", EntityID: alpha.ID, Label: "Alpha", Hop: 1},
		{ID: fmt.Sprintf("person:%d", zulu.ID), Kind: "person", EntityID: zulu.ID, Label: "Zulu", Hop: 1},
		{ID: fmt.Sprintf("organization:%d", organization.ID), Kind: "organization", EntityID: organization.ID, Label: "Example Works", Hop: 2},
	}, graph.Nodes)
}

func TestGetPersonNetworkContextReachesIncomingRelationships(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	incoming := createNetworkPerson(t, f.Store, "Incoming")
	_, err := f.Store.CreateRelationshipTypeContext(t.Context(), store.RelationshipTypeInput{
		Slug: "incoming", ForwardLabel: "source", ReverseLabel: "target",
	})
	require.NoError(err)
	createNetworkRelationship(t, f.Store, incoming.ID, root.ID, "incoming")

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 1})
	require.NoError(err)
	assert.Equal([]string{
		fmt.Sprintf("person:%d", root.ID),
		fmt.Sprintf("person:%d", incoming.ID),
	}, networkNodeIDs(graph.Nodes))
	require.Len(graph.Edges, 1)
	assert.Equal(fmt.Sprintf("person:%d", incoming.ID), graph.Edges[0].SourceNodeID)
	assert.Equal(fmt.Sprintf("person:%d", root.ID), graph.Edges[0].TargetNodeID)
}

func TestGetPersonNetworkContextExpandsOrganizationsToPeople(t *testing.T) {
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	peer := createNetworkPerson(t, f.Store, "Peer")
	organization := createNetworkOrganization(t, f.Store, "Example Works")
	createNetworkEmployment(t, f.Store, root.ID, organization.ID, true)
	createNetworkEmployment(t, f.Store, peer.ID, organization.ID, true)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{
		fmt.Sprintf("person:%d", root.ID),
		fmt.Sprintf("organization:%d", organization.ID),
		fmt.Sprintf("person:%d", peer.ID),
	}, networkNodeIDs(graph.Nodes))
	assert.Len(t, graph.Edges, 2)
}

func TestGetPersonNetworkContextBoundsHighDegreeEmploymentLayerInPublicOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	organization := createNetworkOrganization(t, f.Store, "Example Works")
	createNetworkEmployment(t, f.Store, root.ID, organization.ID, true)
	reads := make([]networkSourceRead, 0, 4)
	restore := f.Store.SetPersonNetworkSourceReadHookForTest(func(limit, count int) {
		reads = append(reads, networkSourceRead{limit: limit, count: count})
	})
	t.Cleanup(restore)

	people := make([]*store.Person, 0, 1000)
	for index := range 1000 {
		person := createNetworkPerson(t, f.Store, fmt.Sprintf("Zed %04d", index))
		people = append(people, person)
		createNetworkEmployment(t, f.Store, person.ID, organization.ID, true)
	}
	earlier := createNetworkPerson(t, f.Store, "AAA Beyond Employment Page")
	createNetworkEmployment(t, f.Store, earlier.ID, organization.ID, true)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.True(graph.Truncated)
	require.Len(graph.Nodes, 250)
	wantHopTwo := []string{fmt.Sprintf("person:%d", earlier.ID)}
	for _, person := range people[:247] {
		wantHopTwo = append(wantHopTwo, fmt.Sprintf("person:%d", person.ID))
	}
	assert.Equal(wantHopTwo, networkNodeIDsAtHop(graph.Nodes, 2),
		"the kept layer prefix follows label order, not employment ID order")

	repeated, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.Equal(graph, repeated)
	assert.Equal([]networkSourceRead{
		{limit: 501, count: 1},
		{limit: 500, count: 500},
		{limit: 501, count: 1},
		{limit: 500, count: 500},
	}, reads)
}

func TestGetPersonNetworkContextAppliesNodeCapAfterLayerOrdering(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	firstParent := createNetworkPerson(t, f.Store, "First Parent")
	secondParent := createNetworkPerson(t, f.Store, "Second Parent")
	createNetworkRelationship(t, f.Store, root.ID, firstParent.ID, "friend")
	createNetworkRelationship(t, f.Store, root.ID, secondParent.ID, "friend")

	people := make([]*store.Person, 0, 248)
	for index := range 248 {
		person := createNetworkPerson(t, f.Store, fmt.Sprintf("Person %03d", index))
		people = append(people, person)
		createNetworkRelationship(t, f.Store, firstParent.ID, person.ID, "friend")
	}
	organization := createNetworkOrganization(t, f.Store, "A Organization")
	createNetworkEmployment(t, f.Store, secondParent.ID, organization.ID, true)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.True(graph.Truncated)
	require.Len(graph.Nodes, 250)
	assert.Contains(networkNodeIDs(graph.Nodes), fmt.Sprintf("organization:%d", organization.ID))
	assert.NotContains(networkNodeIDs(graph.Nodes), fmt.Sprintf("person:%d", people[247].ID))
	assert.Equal([]string{
		fmt.Sprintf("organization:%d", organization.ID),
		fmt.Sprintf("person:%d", people[0].ID),
		fmt.Sprintf("person:%d", people[1].ID),
	}, networkNodeIDsAtHop(graph.Nodes, 2)[:3])
}

func TestGetPersonNetworkContextAppliesEdgeCapSeparately(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	peer := createNetworkPerson(t, f.Store, "Peer")
	reads := make([]networkSourceRead, 0, 2)
	restore := f.Store.SetPersonNetworkSourceReadHookForTest(func(limit, count int) {
		reads = append(reads, networkSourceRead{limit: limit, count: count})
	})
	t.Cleanup(restore)
	for index := range 501 {
		slug := fmt.Sprintf("edge-%03d", index)
		_, err := f.Store.CreateRelationshipTypeContext(t.Context(), store.RelationshipTypeInput{
			Slug: slug, ForwardLabel: slug, ReverseLabel: slug, IsSymmetric: true,
		})
		require.NoError(err)
		createNetworkRelationship(t, f.Store, root.ID, peer.ID, slug)
	}

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 1})
	require.NoError(err)
	assert.True(graph.Truncated)
	assert.Len(graph.Nodes, 2)
	require.Len(graph.Edges, 500)
	wantSlugs := make([]string, 0, 500)
	for index := range 500 {
		wantSlugs = append(wantSlugs, fmt.Sprintf("edge-%03d", index))
	}
	assert.Equal(wantSlugs, networkRelationshipSlugs(graph.Edges))

	repeated, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 1})
	require.NoError(err)
	assert.Equal(graph, repeated)
	assert.Equal([]networkSourceRead{
		{limit: 501, count: 501},
		{limit: 501, count: 501},
	}, reads)
}

// This catches an edge budget charged for rows the traversal already holds:
// every hop-two edge here joins two frontier peers, so a naive read sees it
// once from each endpoint, and the root edges are visible again from every
// peer. Distinct unseen edges stay under the cap, so nothing may be dropped.
func TestGetPersonNetworkContextChargesEdgeBudgetForDistinctUnseenEdgesOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	peers := make([]*store.Person, 0, 249)
	for index := range 249 {
		peer := createNetworkPerson(t, f.Store, fmt.Sprintf("Peer %03d", index))
		peers = append(peers, peer)
		createNetworkRelationship(t, f.Store, root.ID, peer.ID, "friend")
	}
	const cliqueSize = 20
	for left := range cliqueSize {
		for right := left + 1; right < cliqueSize; right++ {
			createNetworkRelationship(t, f.Store, peers[left].ID, peers[right].ID, "friend")
		}
	}
	const wantEdges = 249 + cliqueSize*(cliqueSize-1)/2
	reads := make([]networkSourceRead, 0, 2)
	restore := f.Store.SetPersonNetworkSourceReadHookForTest(func(limit, count int) {
		reads = append(reads, networkSourceRead{limit: limit, count: count})
	})
	t.Cleanup(restore)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 3})
	require.NoError(err)
	assert.False(graph.Truncated)
	assert.Len(graph.Nodes, 250)
	assert.Len(graph.Edges, wantEdges)
	assert.Equal([]networkSourceRead{
		{limit: 501, count: 249},
		{limit: 252, count: cliqueSize * (cliqueSize - 1) / 2},
	}, reads, "already-admitted edges must not be read or charged again")
}

// This catches a truncated layer that keeps edges in frontier or ID order.
// The kept prefix must follow the public ordering: the organization reached
// from the last peer sorts before every person, and the clique edges follow
// their lesser endpoint's label.
func TestGetPersonNetworkContextTruncatesLayerInPublicOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	peers := make([]*store.Person, 0, 248)
	for index := range 248 {
		peer := createNetworkPerson(t, f.Store, fmt.Sprintf("Peer %03d", index))
		peers = append(peers, peer)
		createNetworkRelationship(t, f.Store, root.ID, peer.ID, "friend")
	}
	const cliqueSize = 24
	cliqueEdges := make([][]int64, cliqueSize)
	for left := cliqueSize - 1; left >= 0; left-- {
		for right := cliqueSize - 1; right > left; right-- {
			relationship, err := f.Store.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
				SourcePersonID: peers[left].ID, TargetPersonID: peers[right].ID, TypeSlug: "friend",
				Source: store.ProvenanceUser, Actor: "test",
			})
			require.NoError(err)
			cliqueEdges[left] = append(cliqueEdges[left], relationship.ID)
		}
	}
	organization := createNetworkOrganization(t, f.Store, "Zeta Works")
	createNetworkEmployment(t, f.Store, peers[cliqueSize-1].ID, organization.ID, true)

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.True(graph.Truncated)
	assert.Len(graph.Nodes, 250)
	require.Len(graph.Edges, 500)

	const remaining = 500 - 248
	wantHopTwo := []string{fmt.Sprintf("employment:%d", employmentEdgeID(t, f.Store, peers[cliqueSize-1].ID))}
	for _, ids := range cliqueEdges {
		slices.Sort(ids)
		for _, id := range ids {
			if len(wantHopTwo) == remaining {
				break
			}
			wantHopTwo = append(wantHopTwo, fmt.Sprintf("relationship:%d", id))
		}
	}
	rootID := fmt.Sprintf("person:%d", root.ID)
	gotHopTwo := make([]string, 0, remaining)
	for _, edge := range graph.Edges {
		if edge.SourceNodeID != rootID && edge.TargetNodeID != rootID {
			gotHopTwo = append(gotHopTwo, edge.ID)
		}
	}
	assert.ElementsMatch(wantHopTwo, gotHopTwo)
}

func TestGetPersonNetworkContextIncludesEndedRowsOnlyWhenRequested(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	peer := createNetworkPerson(t, f.Store, "Peer")
	organization := createNetworkOrganization(t, f.Store, "Example Works")
	ended := partialDate(2024, 0, 0)
	_, err := f.Store.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: root.ID, TargetPersonID: peer.ID, TypeSlug: "friend",
		EndDate: &ended, Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	createNetworkEmployment(t, f.Store, peer.ID, organization.ID, false)

	current, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2})
	require.NoError(err)
	assert.Len(current.Nodes, 1)
	assert.Empty(current.Edges)

	history, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 2, IncludeEnded: true})
	require.NoError(err)
	assert.Len(history.Nodes, 3)
	assert.Len(history.Edges, 2)
}

func TestGetPersonNetworkContextUsesEmploymentTitleRoleLabelFallback(t *testing.T) {
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")
	titleOrganization := createNetworkOrganization(t, f.Store, "Title Works")
	roleOrganization := createNetworkOrganization(t, f.Store, "Role Works")
	defaultOrganization := createNetworkOrganization(t, f.Store, "Default Works")
	title, role, empty := "Engineer", "Advisor", ""
	for _, input := range []store.EmploymentInput{
		{PersonID: root.ID, OrganizationID: titleOrganization.ID, Title: &title, Role: &role},
		{PersonID: root.ID, OrganizationID: roleOrganization.ID, Role: &role},
		{PersonID: root.ID, OrganizationID: defaultOrganization.ID, Title: &empty, Role: &empty},
	} {
		current := true
		input.IsCurrent = &current
		input.Source = store.ProvenanceUser
		_, err := f.Store.AddEmploymentContext(t.Context(), input)
		require.NoError(t, err)
	}

	graph, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: 1})
	require.NoError(t, err)
	labels := make(map[string]string, len(graph.Edges))
	for _, edge := range graph.Edges {
		labels[edge.TargetNodeID] = edge.Label
	}
	assert.Equal(t, map[string]string{
		fmt.Sprintf("organization:%d", titleOrganization.ID):   "Engineer",
		fmt.Sprintf("organization:%d", roleOrganization.ID):    "Advisor",
		fmt.Sprintf("organization:%d", defaultOrganization.ID): "employment",
	}, labels)
}

func TestGetPersonNetworkContextValidatesBoundsAndRoot(t *testing.T) {
	f := storetest.New(t)
	root := createNetworkPerson(t, f.Store, "Root")

	for _, depth := range []int{0, 4} {
		_, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID, store.PersonNetworkOptions{Depth: depth})
		require.ErrorIs(t, err, store.ErrPersonNetworkInvalid)
	}
	_, err := f.Store.GetPersonNetworkContext(t.Context(), root.ID+999, store.PersonNetworkOptions{Depth: 1})
	assert.ErrorIs(t, err, store.ErrPersonNotFound)
}

func createNetworkPerson(t *testing.T, s *store.Store, name string) *store.Person {
	t.Helper()
	identifier := strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "@example.test"
	participantID, err := s.EnsureParticipant(identifier, name, "example.test")
	require.NoError(t, err)
	person, _, err := s.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	person, err = s.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &name)
	require.NoError(t, err)
	return person
}

func createNetworkOrganization(t *testing.T, s *store.Store, name string) *store.Organization {
	t.Helper()
	organization, err := s.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: name})
	require.NoError(t, err)
	return organization
}

func createNetworkRelationship(t *testing.T, s *store.Store, sourceID, targetID int64, typeSlug string) {
	t.Helper()
	_, err := s.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: sourceID,
		TargetPersonID: targetID,
		TypeSlug:       typeSlug,
		Source:         store.ProvenanceUser,
		Actor:          "test",
	})
	require.NoError(t, err)
}

func createNetworkEmployment(t *testing.T, s *store.Store, personID, organizationID int64, current bool) {
	t.Helper()
	_, err := s.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID:       personID,
		OrganizationID: organizationID,
		IsCurrent:      &current,
		Source:         store.ProvenanceUser,
	})
	require.NoError(t, err)
}

func networkNodeIDs(nodes []store.NetworkNode) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

func networkNodeIDsAtHop(nodes []store.NetworkNode, hop int) []string {
	ids := make([]string, 0)
	for _, node := range nodes {
		if node.Hop == hop {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

func networkRelationshipSlugs(edges []store.NetworkEdge) []string {
	slugs := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge.RelationshipTypeSlug != nil {
			slugs = append(slugs, *edge.RelationshipTypeSlug)
		}
	}
	return slugs
}

func employmentEdgeID(t *testing.T, s *store.Store, personID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT id FROM employments WHERE person_id = ?`), personID).Scan(&id))
	return id
}
