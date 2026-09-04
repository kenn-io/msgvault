package store

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentTwoStoreCaseVariantAdds covers issue #311's motivating
// scenario: two Store handles on the same database (the serve-plus-CLI
// deployment) concurrently confirm case variants of one email identity.
// The keyed lookup plus the partial unique index must leave exactly one
// row carrying the union of both signal sets.
func TestConcurrentTwoStoreCaseVariantAdds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "race.db")

	first, err := OpenForTest(dbPath)
	require.NoError(err, "open first store")
	t.Cleanup(func() { _ = first.Close() })
	require.NoError(first.InitSchema(), "init schema")

	second, err := OpenForTest(dbPath)
	require.NoError(err, "open second store")
	t.Cleanup(func() { _ = second.Close() })

	src, err := first.GetOrCreateSource("gmail", "two-store-race@example.com")
	require.NoError(err, "GetOrCreateSource")

	stores := []*Store{first, second}
	variants := []string{"Alice@Example.com", "alice@example.com"}
	signals := []string{"manual", "account-identifier", "header"}

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = stores[idx%2].AddAccountIdentity(
				src.ID, variants[idx%2], signals[idx%len(signals)],
			)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(err, "add %d", i)
	}

	identities, err := first.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1, "case variants across two stores must land in one row")
	got := strings.Split(identities[0].SourceSignal, ",")
	for _, want := range signals {
		assert.Contains(got, want, "merged signal set missing %q", want)
	}
}
