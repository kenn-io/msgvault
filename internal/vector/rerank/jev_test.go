package rerank

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/typesafe"
	"go.kenn.io/docbank/document/typesafe/typesafetest"
	"golang.org/x/sync/errgroup"
)

type stubScorer struct {
	result typesafe.Result
	err    error
}

func (s stubScorer) Rerank(context.Context, typesafe.RerankRequest) (typesafe.Result, error) {
	return s.result, s.err
}

func TestNewJevBuildsDocbankClient(t *testing.T) {
	_, err := NewJev("other", "k")
	require.ErrorContains(t, err, "unknown Jev request shape")
	full := strings.Repeat("a", MaxCandidateBytes)
	multibyte := strings.Repeat("界", MaxCandidateBytes/3) + "ab"
	for _, shape := range []string{"per-candidate", "batched"} {
		t.Run(shape+"/KeepsEvalBounds", func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			_, err := NewJev(shape, "k")
			require.NoError(err)
			profile, err := jevProfile(shape)
			require.NoError(err)
			cidrs := profile.EgressPolicy.AllowedCIDRs
			require.Len(cidrs, 2)
			assert.True(cidrs[0].Contains(netip.MustParseAddr("104.18.24.46")))
			assert.True(cidrs[1].Contains(netip.MustParseAddr("2606:4700::6812:182e")))
			fits := slices.Repeat([]string{full}, MaxCandidates)
			fits[1] = multibyte
			require.NoError(typesafe.CheckRequest(profile, typesafe.RerankRequest{Query: "q", Candidates: fits}))
			for _, over := range [][]string{append(fits, "x"), {full + "b"}} {
				err := typesafe.CheckRequest(profile, typesafe.RerankRequest{Query: "q", Candidates: over})
				require.ErrorIs(err, typesafe.ErrCapacityResponse)
				assert.Equal("request bounds exceeded", SafeFailure(err))
			}
		})
	}
}

func TestJevRerank(t *testing.T) {
	candidates := []string{"a longer candidate", "short", "mid text"}
	for shape, requests := range map[string]int{"per-candidate": 3, "batched": 1} {
		t.Run(shape, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			profile, err := jevProfile(shape)
			require.NoError(err)
			fake := typesafetest.New(profile, func(_, candidate string) (float64, error) { return float64(len(candidate)) / 100, nil })
			result, err := (&Jev{scorer: fake}).Rerank(t.Context(), Request{Query: "renewal", Candidates: candidates})
			require.NoError(err)
			assert.Equal([]float64{0.18, 0.05, 0.08}, result.Scores)
			assert.Equal(requests, result.Usage.Requests)
			assert.Equal([]typesafe.RerankRequest{{Query: "renewal", Candidates: candidates}}, fake.Requests())
		})
	}
	t.Run("MapsReceiptTokens", func(t *testing.T) {
		receipt := typesafe.Receipt{RequestShape: typesafe.RequestShapePerCandidate, CandidateCount: 3, InputTokens: 342, OutputTokens: 20}
		result, err := (&Jev{scorer: stubScorer{result: typesafe.Result{Scores: []float64{1, 0, 0.5}, Receipt: receipt}}}).Rerank(t.Context(), Request{})
		require.NoError(t, err)
		assert.Equal(t, Usage{Requests: 3, InputTokens: new(int64(342)), OutputTokens: new(int64(20)), Complete: true}, result.Usage)
	})
	t.Run("FailureLeavesUsageUnknown", func(t *testing.T) {
		failure := errors.New("stub failure")
		result, err := (&Jev{scorer: stubScorer{err: failure}}).Rerank(t.Context(), Request{})
		require.ErrorIs(t, err, failure)
		assert.Equal(t, Result{}, result)
	})
}

// wavedScorer sends one candidate per fake call under the profile's deadline and concurrency, as Docbank's per-candidate client does.
type wavedScorer struct {
	profile typesafe.Profile
	fake    *typesafetest.Fake
}

func (s wavedScorer) Rerank(ctx context.Context, request typesafe.RerankRequest) (typesafe.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, s.profile.RequestTimeout)
	defer cancel()
	scores := make([]float64, len(request.Candidates))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(s.profile.MaxConcurrentCalls)
	for i, candidate := range request.Candidates {
		group.Go(func() error {
			result, err := s.fake.Rerank(groupCtx, typesafe.RerankRequest{Query: request.Query, Candidates: []string{candidate}})
			if err != nil {
				return fmt.Errorf("fake call: %w", err)
			}
			scores[i] = result.Scores[0]
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return typesafe.Result{}, fmt.Errorf("waves: %w", err)
	}
	return typesafe.Result{Scores: scores, Receipt: typesafe.Receipt{RequestShape: typesafe.RequestShapePerCandidate, CandidateCount: len(scores)}}, nil
}

func TestJevPerCandidateSlowWavesFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		profile, err := jevProfile("per-candidate")
		require.NoError(err)
		fake := typesafetest.New(profile, func(string, string) (float64, error) {
			time.Sleep(8 * time.Second)
			return 0.5, nil
		})
		candidates := make([]string, MaxCandidates)
		for i := range candidates {
			candidates[i] = fmt.Sprintf("candidate %d", i)
		}
		start := time.Now()
		result, err := (&Jev{scorer: wavedScorer{profile: profile, fake: fake}}).Rerank(t.Context(), Request{Query: "renewal", Candidates: candidates})
		require.NoError(err, "four waves of 8-second calls must fit the ranking deadline")
		assert.Len(result.Scores, MaxCandidates)
		assert.Equal(MaxCandidates, result.Usage.Requests)
		assert.Equal(32*time.Second, time.Since(start))
	})
}

func TestSafeFailureCategories(t *testing.T) {
	for want, err := range map[string]error{
		"provider returned HTTP 429":            &typesafe.ProviderError{Kind: typesafe.ErrTransientResponse, StatusCode: 429},
		"invalid provider response":             fmt.Errorf("typesafe rerank: provider calls failed: %w", &typesafe.ProviderError{Kind: typesafe.ErrPermanentResponse}),
		"request bounds exceeded":               &typesafe.ProviderError{Kind: typesafe.ErrCapacityResponse},
		"provider timeout or transport failure": &typesafe.ProviderError{Kind: typesafe.ErrTransientResponse},
		"provider timeout or cancellation":      fmt.Errorf("x: %w", context.Canceled),
		"provider request failed":               errors.New("secret-key query body text"),
	} {
		assert.Equal(t, want, SafeFailure(err))
	}
	assert.Equal(t, "invalid provider response", SafeFailure(fmt.Errorf("%w: n", ErrInvalidResponse)))
}
