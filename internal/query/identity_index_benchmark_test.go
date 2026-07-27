package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

type relationshipBenchmarkProfile struct {
	CumulativeRowsScanned int64 `json:"cumulative_rows_scanned"`
	PeakSpillBytes        int64 `json:"system_peak_temp_dir_size"`
}

type relationshipColdOperation func(
	context.Context,
	*DuckDBEngine,
) (int64, error)

// BenchmarkRelationshipIndexCold measures the production cache created by
// scripts/benchmark-relationships-index.sh. A fresh engine owns every timed
// query, so DuckDB buffers and the engine's readiness memo cannot make a later
// iteration warm. The scale switch is deliberately opt-in because the fixture
// is 2.5 million messages and six million direct participant edges.
func BenchmarkRelationshipIndexCold(b *testing.B) {
	requirementsForTest := require.New(b)
	home := requireRelationshipScaleBenchmarkHome(b)
	analyticsDir := filepath.Join(home, "analytics")
	state, err := ReadCacheSyncState(analyticsDir)
	requirementsForTest.NoError(err)
	requirementsForTest.Equal(relationshipScaleMessages, state.LastMessageID)
	requirementsForTest.Equal(relationshipScaleMessages, state.Stats.TotalMessages)
	validationEngine, err := NewDuckDBEngine(analyticsDir, "", nil)
	requirementsForTest.NoError(err)
	var directEdges, participants int64
	requirementsForTest.NoError(validationEngine.db.QueryRowContext(
		context.Background(),
		"SELECT count(*) FROM read_parquet(?)",
		validationEngine.parquetPath(identityindex.DatasetDirectEdges),
	).Scan(&directEdges))
	requirementsForTest.NoError(validationEngine.db.QueryRowContext(
		context.Background(),
		"SELECT count(*) FROM read_parquet(?)",
		validationEngine.parquetPath("participants"),
	).Scan(&participants))
	requirementsForTest.Equal(relationshipScaleEdges, directEdges)
	requirementsForTest.Equal(relationshipScaleParticipants, participants)
	requirementsForTest.NoError(validationEngine.Close())

	benchmarkRelationshipColdOperation(b, home, "relationships",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.Relationships(ctx, RelationshipsRequest{
				Now:   time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
				Limit: 100,
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	benchmarkRelationshipColdOperation(b, home, "people-search",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.SearchPeople(ctx, PersonSearchRequest{
				Sort: SortSpec{Field: "activity_count", Direction: "desc"},
				Page: PageSpec{Limit: 100},
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	benchmarkRelationshipColdOperation(b, home, "domain-search",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.SearchDomains(ctx, DomainSearchRequest{
				Sort: SortSpec{Field: "activity_count", Direction: "desc"},
				Page: PageSpec{Limit: 100},
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	benchmarkRelationshipColdOperation(b, home, "filtered-people",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.SearchPeople(ctx, PersonSearchRequest{
				Explore: ExploreRequest{Context: Context{
					SourceIDs:      []int64{1},
					ParticipantIDs: []int64{101},
					MessageTypes:   []string{"email"},
					Deletion:       DeletionActive,
				}},
				Sort: SortSpec{Field: "activity_count", Direction: "desc"},
				Page: PageSpec{Limit: 100},
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	benchmarkRelationshipColdOperation(b, home, "filtered-relationships",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.Relationships(ctx, RelationshipsRequest{
				Context: Context{
					SourceIDs:      []int64{1},
					ParticipantIDs: []int64{101},
					MessageTypes:   []string{"email"},
					Deletion:       DeletionActive,
				},
				Now:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
				ShowAll: true,
				Limit:   100,
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
}

func benchmarkRelationshipColdOperation(
	b *testing.B,
	home, name string,
	operation relationshipColdOperation,
) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		requirementsForTest := require.New(b)
		var rowsScanned, spillBytes, returnedRows int64
		for b.Loop() {
			b.StopTimer()
			engine, err := NewDuckDBEngine(
				filepath.Join(home, "analytics"),
				"",
				nil,
				DuckDBOptions{DisableSQLiteScanner: true},
			)
			requirementsForTest.NoError(err)
			profilePath := filepath.Join(engine.tempDirectory, "profile.json")
			quotedProfilePath := strings.ReplaceAll(profilePath, "'", "''")
			_, err = engine.db.ExecContext(
				context.Background(),
				"PRAGMA enable_profiling='json'",
			)
			requirementsForTest.NoError(err)
			_, err = engine.db.ExecContext(
				context.Background(),
				"SET profiling_output='"+quotedProfilePath+"'",
			)
			requirementsForTest.NoError(err)

			b.StartTimer()
			rowCount, queryErr := operation(context.Background(), engine)
			b.StopTimer()
			requirementsForTest.NoError(queryErr)
			returnedRows += rowCount

			_, err = engine.db.ExecContext(
				context.Background(),
				"PRAGMA disable_profiling",
			)
			requirementsForTest.NoError(err)
			profileData, err := os.ReadFile(profilePath)
			requirementsForTest.NoError(err)
			var profile relationshipBenchmarkProfile
			requirementsForTest.NoError(json.Unmarshal(profileData, &profile))
			rowsScanned += profile.CumulativeRowsScanned
			spillBytes += max(profile.PeakSpillBytes,
				relationshipBenchmarkDirectoryBytes(b, engine.tempDirectory))
			requirementsForTest.NoError(engine.Close())
			b.StartTimer()
		}
		b.ReportMetric(float64(rowsScanned)/float64(b.N), "rows-scanned/op")
		b.ReportMetric(float64(spillBytes)/float64(b.N), "spill-bytes/op")
		b.ReportMetric(float64(returnedRows)/float64(b.N), "rows-returned/op")
	})
}

func relationshipBenchmarkDirectoryBytes(tb testing.TB, root string) int64 {
	tb.Helper()
	var total int64
	require.NoError(tb, filepath.WalkDir(root, func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil {
			return fmt.Errorf("walk relationship benchmark directory: %w", err)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat relationship benchmark file: %w", err)
		}
		total += info.Size()
		return nil
	}))
	return total
}
