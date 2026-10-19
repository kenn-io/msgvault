package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierProjectionRunFixture(t *testing.T) (*store.Store, int64, store.PersonTierPageRequest) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("projection-tier@example.org", "Synthetic Projection", "example.org"))
	require.NoError(t, err)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	return f.Store, person.ID, store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
}

func completeTierProjectionSettings(t *testing.T, s *store.Store, zone string, limit int) {
	t.Helper()
	tz, err := s.ClaimActivityTimezoneTransitionContext(t.Context(), zone)
	require.NoError(t, err)
	if tz.Active {
		require.NoError(t, s.CompleteActivityTimezoneTransitionContext(t.Context(), tz))
	}
	direct, err := s.ClaimActivityDirectLimitTransitionContext(t.Context(), limit)
	require.NoError(t, err)
	if direct.Active {
		require.NoError(t, s.CompleteActivityDirectLimitTransitionContext(t.Context(), direct))
	}
}

func TestPersonTierCompletedProjectionChangeSupersedesRun(t *testing.T) {
	for _, kind := range []string{"manual", "scheduled"} {
		for _, mode := range []string{"apply", "record_only"} {
			for _, setting := range []string{"timezone", "direct_limit", "unchanged"} {
				t.Run(kind+"/"+mode+"/"+setting, func(t *testing.T) {
					s, _, req := tierProjectionRunFixture(t)
					participant, err := s.EnsureParticipant("next-projection@example.org", "Next Projection", "example.org")
					require.NoError(t, err)
					_, _, err = s.CreatePersonFromParticipant(participant)
					require.NoError(t, err)
					completeTierProjectionSettings(t, s, "UTC", 25)
					run, err := s.StartPersonTierRunContext(t.Context(), req, kind, mode)
					require.NoError(t, err)
					first, err := s.ProcessPersonTierRunPageContext(t.Context(), *run, 1, "operator")
					require.NoError(t, err)
					require.NotEmpty(t, first.NextCursor)
					run, err = s.PersonTierRunContext(t.Context(), kind)
					require.NoError(t, err)
					require.NotNil(t, run)
					before := tierProjectionRowCounts(t, s)
					switch setting {
					case "timezone":
						completeTierProjectionSettings(t, s, "America/Los_Angeles", 25)
					case "direct_limit":
						completeTierProjectionSettings(t, s, "UTC", 1)
					default:
						completeTierProjectionSettings(t, s, "UTC", 25)
					}
					page, err := s.ProcessPersonTierRunPageContext(t.Context(), *run, 1, "operator")
					if setting == "unchanged" {
						require.NoError(t, err)
						require.Len(t, page.People, 1)
						assert.Empty(t, page.NextCursor)
						counts := tierProjectionRowCounts(t, s)
						if mode == "apply" {
							assert.Equal(t, 2, counts["person_tier_state"])
						} else {
							assert.Equal(t, 2, counts["person_tier_observations"])
						}
						return
					}
					require.ErrorIs(t, err, store.ErrPersonTierRunSuperseded)
					assert.Equal(t, before, tierProjectionRowCounts(t, s))
					remaining, err := s.PersonTierRunContext(t.Context(), kind)
					require.NoError(t, err)
					if kind == "scheduled" {
						assert.Nil(t, remaining)
					} else {
						assert.NotNil(t, remaining)
					}
				})
			}
		}
	}
}

func TestPersonTierProjectionChangeBetweenPlanAndWrite(t *testing.T) {
	for _, setting := range []string{"timezone", "direct_limit"} {
		t.Run(setting, func(t *testing.T) {
			s, _, req := tierProjectionRunFixture(t)
			completeTierProjectionSettings(t, s, "UTC", 25)
			run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
			require.NoError(t, err)
			page, err := s.PlanPersonTiersContext(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, page.People, 1)
			before := tierProjectionRowCounts(t, s)
			if setting == "timezone" {
				completeTierProjectionSettings(t, s, "America/Los_Angeles", 25)
			} else {
				completeTierProjectionSettings(t, s, "UTC", 1)
			}
			require.ErrorIs(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]), store.ErrPersonTierRunSuperseded)
			assert.Equal(t, before, tierProjectionRowCounts(t, s))
		})
	}
}

func TestPersonTierReadsRejectMismatchedProjection(t *testing.T) {
	for _, setting := range []string{"timezone", "direct_limit"} {
		t.Run(setting, func(t *testing.T) {
			s, personID, req := tierProjectionRunFixture(t)
			completeTierProjectionSettings(t, s, "UTC", 25)
			if setting == "timezone" {
				req.Timezone = "America/Los_Angeles"
			} else {
				req.MaxDirect = 1
			}
			before := tierProjectionRowCounts(t, s)
			_, err := s.PlanPersonTiersContext(t.Context(), req)
			assert.ErrorIs(t, err, store.ErrInvalidPersonTierRequest)
			_, err = s.ExplainPersonTierContext(t.Context(), personID, req)
			assert.ErrorIs(t, err, store.ErrInvalidPersonTierRequest)
			assert.Equal(t, before, tierProjectionRowCounts(t, s))
		})
	}
}

func tierProjectionRowCounts(t *testing.T, s *store.Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"person_tier_state", "person_tier_observations", "person_tier_events", "person_tracking", "person_brief_enrollments"} {
		var count int
		require.NoError(t, s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		counts[table] = count
	}
	return counts
}
