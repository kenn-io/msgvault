package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// scopedFakeCoverage records the scope arguments MissingCountScoped
// receives so the test can prove the job forwards its full build scope.
type scopedFakeCoverage struct {
	messageTypes []string
	sourceIDs    []int64
	missing      int64
}

func (c *scopedFakeCoverage) MissingCount(context.Context, int64) (int64, error) {
	return -1, errors.New("unscoped path must not be used for a scoped job")
}

func (c *scopedFakeCoverage) MissingCountScoped(_ context.Context, _ int64, messageTypes []string, sourceIDs []int64) (int64, error) {
	c.messageTypes = messageTypes
	c.sourceIDs = sourceIDs
	return c.missing, nil
}

// TestEmbedJobMissingCountForwardsScope pins the account dimension: a job
// built with a source-scoped BuildScope must pass those source IDs to the
// coverage store, or the daemon's activation gate would count the whole
// corpus and the scoped generation could never activate.
func TestEmbedJobMissingCountForwardsScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cov := &scopedFakeCoverage{missing: 5}
	job := &EmbedJob{
		Store:      cov,
		BuildScope: vector.NewBuildScope([]string{"SMS"}, []int64{7, 3}),
	}
	got, err := job.missingCount(context.Background(), 12)
	require.NoError(err)
	assert.Equal(int64(5), got)
	assert.Equal([]string{"sms"}, cov.messageTypes, "message types normalized")
	assert.Equal([]int64{3, 7}, cov.sourceIDs, "source IDs normalized and forwarded")
}

func TestEmbedJobScopeChangeFailsClosedBeforeWorkerRuns(t *testing.T) {
	assert := assert.New(t)
	runner := &fakeRunner{}
	job := &EmbedJob{
		Worker:     runner,
		Backend:    &fakeBackend{},
		BuildScope: vector.NewBuildScope(nil, []int64{7}),
		ResolveBuildScope: func() (vector.BuildScope, error) {
			return vector.NewBuildScope(nil, []int64{9}), nil
		},
	}

	job.Run(context.Background())
	assert.Equal(0, runner.runCalls, "changed source mapping must stop before embedding")
	assert.Equal(0, runner.reclaimCalls, "changed source mapping must stop before any worker write")
}
