package emailfamily

import (
	"fmt"
	"slices"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func workMember(id, source int64, rawHeaders string) Member {
	return Member{MessageID: id, SourceID: source, SourceType: "gmail", Linkage: mime.ExtractEmailLinkage([]byte(rawHeaders + "\r\n\r\n[QA] body"))}
}

func TestEmailWorkPartitionPreservesCopiesAndExplicitForward(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	members := []Member{
		workMember(1, 1, "Message-ID: <original@example.test>\r\nSubject: [QA] Same subject"),
		workMember(2, 2, "Message-ID: <original@example.test>\r\nSubject: [QA] Same subject"),
		workMember(3, 3, "Message-ID: <reply@example.test>\r\nIn-Reply-To: <original@example.test>"),
		workMember(4, 1, "Message-ID: <unrelated@example.test>\r\nSubject: [QA] Same subject"),
	}
	forwarded := workMember(5, 2, "Message-ID: <forward@example.test>")
	forwarded.Linkage.AttachedOriginals = []string{"original@example.test"}
	members = append(members, forwarded)
	got, err := Partition(members, nil)
	requirements.NoError(err)
	assertions.Equal([][]int64{{1, 2, 3, 5}, {4}}, got)
	// Removing the forward's actual linkage restores its independent row.
	members[4].Linkage.AttachedOriginals = nil
	got, err = Partition(members, nil)
	requirements.NoError(err)
	assertions.Equal([][]int64{{1, 2, 3}, {4}, {5}}, got)
}

func TestEmailWorkPartitionDoesNotUseExcludedIntermediate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	a := workMember(1, 1, "Message-ID: <a@example.test>\r\nIn-Reply-To: <b@example.test>")
	b := workMember(2, 2, "Message-ID: <b@example.test>\r\nIn-Reply-To: <c@example.test>")
	c := workMember(3, 3, "Message-ID: <c@example.test>")
	full, err := Partition([]Member{a, b, c}, nil)
	requirements.NoError(err)
	assertions.Equal([][]int64{{1, 2, 3}}, full)
	scoped, err := Partition([]Member{a, c}, nil)
	requirements.NoError(err)
	assertions.Equal([][]int64{{1}, {3}}, scoped)
}

func TestEmailWorkPartitionProviderThreadsStaySourcePinned(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	a := workMember(1, 1, "Message-ID: <a@example.test>")
	a.ProviderThreadID = "same-provider-token"
	b := workMember(2, 1, "Message-ID: <b@example.test>")
	b.ProviderThreadID = "same-provider-token"
	c := workMember(3, 2, "Message-ID: <c@example.test>")
	c.ProviderThreadID = "same-provider-token"
	got, err := Partition([]Member{a, b, c}, nil)
	requirements.NoError(err)
	assertions.Equal([][]int64{{1, 2}, {3}}, got)
}

func TestEmailWorkPartitionCyclesMissingParentsAndOrderConservation(t *testing.T) {
	members := []Member{
		workMember(1, 1, "Message-ID: <a@example.test>\r\nIn-Reply-To: <b@example.test>"),
		workMember(2, 2, "Message-ID: <b@example.test>\r\nIn-Reply-To: <a@example.test>"),
		workMember(3, 3, "Message-ID: <c@example.test>\r\nReferences: <not-archived@example.test>"),
		workMember(4, 2, "Message-ID: <d@example.test>\r\nReferences: <not-archived@example.test>"),
		workMember(5, 1, "Subject: [QA] no identity"),
	}
	want := [][]int64{{1, 2}, {3, 4}, {5}}
	require.NoError(t, quick.Check(func(seed int64) bool {
		reordered := slices.Clone(members)
		// The generated seed chooses a deterministic permutation; no random
		// identifiers or security tokens are being created by this property.
		for i := len(reordered) - 1; i > 0; i-- {
			j := int(uint64(seed) >> uint(i*8) % uint64(i+1))
			reordered[i], reordered[j] = reordered[j], reordered[i]
		}
		got, err := Partition(reordered, nil)
		if err != nil || len(got) != len(want) {
			return false
		}
		for i := range want {
			if !slices.Equal(want[i], got[i]) {
				return false
			}
		}
		return true
	}, &quick.Config{MaxCount: 500}))
}

func TestPartitionManualEndpointsMustAllSurvive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	members := []Member{workMember(1, 1, ""), workMember(2, 2, ""), workMember(3, 3, "")}
	got, err := Partition(members, [][]int64{{1, 2, 99}})
	require.NoError(err)
	assert.Equal([][]int64{{1}, {2}, {3}}, got)
	got, err = Partition(members, [][]int64{{1, 2}, {2, 3}})
	require.NoError(err)
	assert.Equal([][]int64{{1, 2, 3}}, got)
	members[0].SourceType = "imap"
	members[1].SourceType = "imap"
	members[1].SourceID = 1
	members[0].ProviderThreadID = "same"
	members[1].ProviderThreadID = "same"
	got, err = Partition(members, nil)
	require.NoError(err)
	assert.Equal([][]int64{{1}, {2}, {3}}, got)
}

func FuzzPartitionConservesMembers(f *testing.F) {
	f.Add([]byte{1, 2, 1, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, keys []byte) {
		if len(keys) > 500 {
			keys = keys[:500]
		}
		members := make([]Member, len(keys))
		for i, key := range keys {
			members[i] = workMember(int64(i+1), 1, fmt.Sprintf("Message-ID: <id%d@example.test>", key))
		}
		groups, err := Partition(members, nil)
		require.NoError(t, err)
		var ids []int64
		for _, group := range groups {
			ids = append(ids, group...)
		}
		slices.Sort(ids)
		require.Len(t, ids, len(keys))
		for i, id := range ids {
			assert.Equal(t, int64(i+1), id)
		}
	})
}
