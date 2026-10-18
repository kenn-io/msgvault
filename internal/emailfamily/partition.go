package emailfamily

import (
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/mime"
)

// Member is a message from the already-authorized selected population.
// ProviderThreadID is supplied only for a source-native authoritative email
// thread, never a subject-based or historical MIME-parser guess.
type Member struct {
	SourceType       string            `json:"source_type"`
	MessageID        int64             `json:"message_id"`
	SourceID         int64             `json:"source_id"`
	ProviderThreadID string            `json:"provider_thread_id"`
	Linkage          mime.EmailLinkage `json:"linkage"`
}

// Partition computes families from an already-authorized population. Shared
// explicit identifiers join across sources; authoritative Gmail threads join
// within one source. A manual link requires every endpoint to be present.
// Each copy appears once, and both groups and members sort by message ID.
func Partition(members []Member, links [][]int64) ([][]int64, error) {
	parents := make(map[int64]int64, len(members))
	var ids []int64
	for _, member := range members {
		if member.SourceID < 1 || member.MessageID < 1 {
			return nil, errors.New("email family members require positive message and source IDs")
		}
		if _, exists := parents[member.MessageID]; exists {
			return nil, errors.New("email family population contains a repeated message ID")
		}
		parents[member.MessageID] = member.MessageID
		ids = append(ids, member.MessageID)
	}
	root := func(id int64) int64 {
		for parents[id] != id {
			parents[id] = parents[parents[id]]
			id = parents[id]
		}
		return id
	}
	union := func(a, b int64) {
		a = root(a)
		b = root(b)
		if a > b {
			a, b = b, a
		}
		parents[b] = a
	}
	identityOwner := map[string]int64{}
	type sourceThread struct {
		sourceID int64
		threadID string
	}
	threadOwner := map[sourceThread]int64{}
	for _, member := range members {
		if member.SourceType == "gmail" && member.ProviderThreadID != "" {
			key := sourceThread{member.SourceID, member.ProviderThreadID}
			if other, found := threadOwner[key]; found {
				union(member.MessageID, other)
			} else {
				threadOwner[key] = member.MessageID
			}
		}
		if member.Linkage.Version != mime.EmailLinkageVersion {
			continue
		}
		evidenceIDs := []string{member.Linkage.MessageID}
		evidenceIDs = append(evidenceIDs, member.Linkage.Replies...)
		evidenceIDs = append(evidenceIDs, member.Linkage.References...)
		evidenceIDs = append(evidenceIDs, member.Linkage.AttachedOriginals...)
		for _, identity := range evidenceIDs {
			if identity == "" {
				continue
			}
			if other, found := identityOwner[identity]; found {
				union(member.MessageID, other)
			} else {
				identityOwner[identity] = member.MessageID
			}
		}
	}
	for _, link := range links {
		complete := len(link) > 1
		for _, id := range link {
			_, present := parents[id]
			complete = complete && present
		}
		if complete {
			for _, id := range link[1:] {
				union(link[0], id)
			}
		}
	}
	slices.Sort(ids)
	components := map[int64][]int64{}
	var representatives []int64
	for _, id := range ids {
		representative := root(id)
		if _, found := components[representative]; !found {
			representatives = append(representatives, representative)
		}
		components[representative] = append(components[representative], id)
	}
	result := make([][]int64, 0, len(representatives))
	for _, representative := range representatives {
		result = append(result, components[representative])
	}
	return result, nil
}
