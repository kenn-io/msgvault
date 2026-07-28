package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

type relationshipBenchmarkProfile struct {
	CumulativeRowsScanned int64                           `json:"cumulative_rows_scanned"`
	PeakSpillBytes        int64                           `json:"system_peak_temp_dir_size"`
	QueryName             string                          `json:"query_name"`
	Children              []relationshipBenchmarkOperator `json:"children"`
}

type relationshipBenchmarkOperator struct {
	Name        string                          `json:"operator_name"`
	Type        string                          `json:"operator_type"`
	RowsScanned int64                           `json:"operator_rows_scanned"`
	ExtraInfo   map[string]any                  `json:"extra_info,omitempty"`
	Children    []relationshipBenchmarkOperator `json:"children,omitempty"`
}

type relationshipBenchmarkDatasetScan struct {
	Dataset     string `json:"dataset"`
	Operator    string `json:"operator"`
	RowsScanned int64  `json:"rows_scanned"`
}

type relationshipBenchmarkStatementProfile struct {
	Index       int                             `json:"index"`
	QueryName   string                          `json:"query_name"`
	RowsScanned int64                           `json:"rows_scanned"`
	SpillBytes  int64                           `json:"spill_bytes"`
	Operators   []relationshipBenchmarkOperator `json:"operators"`
}

type relationshipBenchmarkOperationProfile struct {
	Name                 string                                  `json:"name"`
	RowsScanned          int64                                   `json:"rows_scanned"`
	SpillBytes           int64                                   `json:"spill_bytes"`
	RowsReturned         int64                                   `json:"rows_returned"`
	Statements           []relationshipBenchmarkStatementProfile `json:"statements"`
	DatasetOperatorScans []relationshipBenchmarkDatasetScan      `json:"dataset_operator_scans"`
}

type relationshipBenchmarkEvidence struct {
	Version    int                                     `json:"version"`
	Operations []relationshipBenchmarkOperationProfile `json:"operations"`
}

const relationshipBenchmarkEvidencePathEnv = "MSGVAULT_RELATIONSHIPS_BENCH_PROFILE"

type relationshipColdOperation func(
	context.Context,
	*DuckDBEngine,
) (int64, error)

func TestWriteRelationshipBenchmarkEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "profile.json")
	evidence := relationshipBenchmarkEvidence{
		Version: 1,
		Operations: []relationshipBenchmarkOperationProfile{
			{
				Name:         "source-only-people",
				RowsScanned:  2_500_000,
				SpillBytes:   4096,
				RowsReturned: 100,
			},
			{
				Name:         "date-window-relationships",
				RowsScanned:  250_000,
				SpillBytes:   0,
				RowsReturned: 100,
			},
		},
	}

	requirements.NoError(writeRelationshipBenchmarkEvidence(path, evidence))
	data, err := os.ReadFile(path)
	requirements.NoError(err)
	var decoded relationshipBenchmarkEvidence
	requirements.NoError(json.Unmarshal(data, &decoded))
	assertions.Equal(evidence, decoded)
	assertions.NotContains(string(data), "\n",
		"the harness embeds this artifact directly in its one-line JSON result")
}

func TestRelationshipBenchmarkDirectoryBytesCountsEverySpillDirectoryFile(t *testing.T) {
	requirements := require.New(t)
	root := t.TempDir()
	requirements.NoError(os.WriteFile(
		filepath.Join(root, "profile.json"),
		make([]byte, 100),
		0o600,
	))
	requirements.NoError(os.WriteFile(
		filepath.Join(root, "duckdb-spill.tmp"),
		make([]byte, 200),
		0o600,
	))

	assert.Equal(t, int64(300), relationshipBenchmarkDirectoryBytes(t, root))
}

func TestAggregateRelationshipBenchmarkProfilesIncludesEveryStatementAndDatasetOperator(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	profiles := []relationshipBenchmarkProfile{
		{
			QueryName:             "candidate preselection",
			CumulativeRowsScanned: 12,
			PeakSpillBytes:        3,
			Children: []relationshipBenchmarkOperator{{
				Name:        "READ_PARQUET",
				Type:        "TABLE_SCAN",
				RowsScanned: 12,
				ExtraInfo: map[string]any{
					"Filename(s)": "/cache/identity_entry_facts/year=2024/data.parquet",
				},
			}},
		},
		{
			QueryName:             "final aggregation",
			CumulativeRowsScanned: 20,
			PeakSpillBytes:        5,
			Children: []relationshipBenchmarkOperator{{
				Name: "HASH_JOIN",
				Type: "HASH_JOIN",
				Children: []relationshipBenchmarkOperator{{
					Name:        "READ_PARQUET",
					Type:        "TABLE_SCAN",
					RowsScanned: 20,
					ExtraInfo: map[string]any{
						"Filename(s)": "/cache/identity_directory/data.parquet",
					},
				}},
			}},
		},
	}

	got := aggregateRelationshipBenchmarkProfiles(profiles)

	assert.Equal(int64(32), got.RowsScanned)
	assert.Equal(int64(5), got.SpillBytes)
	require.Len(got.Statements, 2)
	assert.Equal("candidate preselection", got.Statements[0].QueryName)
	assert.Equal("final aggregation", got.Statements[1].QueryName)
	assert.Equal([]relationshipBenchmarkDatasetScan{
		{Dataset: identityindex.DatasetDirectory, Operator: "READ_PARQUET", RowsScanned: 20},
		{Dataset: identityindex.DatasetEntryFacts, Operator: "READ_PARQUET", RowsScanned: 12},
	}, got.DatasetOperatorScans)
}

func TestDuckDBStatementProfilerRotatesOutputForEveryMeasuredQuery(t *testing.T) {
	require := require.New(t)
	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(engine.Close()) })
	profileDir := t.TempDir()
	assert.NotEqual(t, filepath.Clean(engine.tempDirectory), filepath.Clean(profileDir))
	engine.relationshipBenchmarkProfileDir = profileDir
	_, err = engine.db.ExecContext(t.Context(), "PRAGMA enable_profiling='json'")
	require.NoError(err)

	for _, queryText := range []string{
		"SELECT i FROM range(3) t(i)",
		"SELECT i FROM range(5) t(i)",
	} {
		func() {
			rows, queryErr := engine.profiledQueryContext(t.Context(), queryText)
			require.NoError(queryErr)
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var value int64
				require.NoError(rows.Scan(&value))
			}
			require.NoError(rows.Err())
		}()
	}
	_, err = engine.db.ExecContext(t.Context(), "PRAGMA disable_profiling")
	require.NoError(err)

	files, err := filepath.Glob(filepath.Join(profileDir, "statement-*.json"))
	require.NoError(err)
	require.Len(files, 2)
}

func writeRelationshipBenchmarkEvidence(
	path string,
	evidence relationshipBenchmarkEvidence,
) error {
	data, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode relationship benchmark evidence: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write relationship benchmark evidence: %w", err)
	}
	return nil
}

func aggregateRelationshipBenchmarkProfiles(
	profiles []relationshipBenchmarkProfile,
) relationshipBenchmarkOperationProfile {
	result := relationshipBenchmarkOperationProfile{
		Statements: make([]relationshipBenchmarkStatementProfile, 0, len(profiles)),
	}
	scans := make(map[string]int64)
	for index, profile := range profiles {
		result.RowsScanned += profile.CumulativeRowsScanned
		result.SpillBytes = max(result.SpillBytes, profile.PeakSpillBytes)
		result.Statements = append(result.Statements, relationshipBenchmarkStatementProfile{
			Index:       index + 1,
			QueryName:   profile.QueryName,
			RowsScanned: profile.CumulativeRowsScanned,
			SpillBytes:  profile.PeakSpillBytes,
			Operators:   profile.Children,
		})
		collectRelationshipBenchmarkDatasetScans(profile.Children, scans)
	}
	for key, rowsScanned := range scans {
		parts := strings.SplitN(key, "\x00", 2)
		result.DatasetOperatorScans = append(
			result.DatasetOperatorScans,
			relationshipBenchmarkDatasetScan{
				Dataset:     parts[0],
				Operator:    parts[1],
				RowsScanned: rowsScanned,
			},
		)
	}
	sort.Slice(result.DatasetOperatorScans, func(i, j int) bool {
		left, right := result.DatasetOperatorScans[i], result.DatasetOperatorScans[j]
		if left.Dataset != right.Dataset {
			return left.Dataset < right.Dataset
		}
		return left.Operator < right.Operator
	})
	return result
}

func collectRelationshipBenchmarkDatasetScans(
	operators []relationshipBenchmarkOperator,
	scans map[string]int64,
) {
	for _, operator := range operators {
		if operator.RowsScanned > 0 {
			if dataset := relationshipBenchmarkOperatorDataset(operator); dataset != "" {
				scans[dataset+"\x00"+operator.Name] += operator.RowsScanned
			}
		}
		collectRelationshipBenchmarkDatasetScans(operator.Children, scans)
	}
}

func relationshipBenchmarkOperatorDataset(
	operator relationshipBenchmarkOperator,
) string {
	filename, ok := operator.ExtraInfo["Filename(s)"]
	if !ok {
		return ""
	}
	filenameText := fmt.Sprint(filename)
	for _, dataset := range RequiredParquetDirs {
		if strings.Contains(
			filepath.ToSlash(filenameText),
			"/"+dataset+"/",
		) {
			return dataset
		}
	}
	return ""
}

// BenchmarkRelationshipIndexCold measures the production cache created by
// scripts/benchmark-relationships-index.sh. A fresh engine owns every timed
// query, so DuckDB buffers and the engine's readiness memo cannot make a later
// iteration warm. The scale switch is deliberately opt-in because the fixture
// is 2.5 million messages and six million direct participant edges.
func BenchmarkRelationshipIndexCold(b *testing.B) {
	requirementsForTest := require.New(b)
	evidence := relationshipBenchmarkEvidence{Version: 1}
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

	addOperation := func(name string, operation relationshipColdOperation) {
		evidence.Operations = append(
			evidence.Operations,
			benchmarkRelationshipColdOperation(b, home, name, operation),
		)
	}
	addOperation("relationships",
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
	addOperation("people-search",
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
	addOperation("domain-search",
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
	addOperation("source-only-people",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.SearchPeople(ctx, PersonSearchRequest{
				Explore: ExploreRequest{Context: Context{SourceIDs: []int64{1}}},
				Sort:    SortSpec{Field: "activity_count", Direction: "desc"},
				Page:    PageSpec{Limit: 100},
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	addOperation("date-window-relationships",
		func(ctx context.Context, engine *DuckDBEngine) (int64, error) {
			response, queryErr := engine.Relationships(ctx, RelationshipsRequest{
				Context: Context{After: &after, Before: &before},
				Now:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
				ShowAll: true,
				Limit:   100,
			})
			if queryErr != nil {
				return 0, queryErr
			}
			return int64(len(response.Rows)), nil
		})
	addOperation("filtered-people",
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
	addOperation("filtered-relationships",
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
	if evidencePath := os.Getenv(relationshipBenchmarkEvidencePathEnv); evidencePath != "" {
		requirementsForTest.NoError(
			writeRelationshipBenchmarkEvidence(evidencePath, evidence),
		)
	}
}

func benchmarkRelationshipColdOperation(
	b *testing.B,
	home, name string,
	operation relationshipColdOperation,
) relationshipBenchmarkOperationProfile {
	b.Helper()
	result := relationshipBenchmarkOperationProfile{Name: name}
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
			profileDir := b.TempDir()
			engine.relationshipBenchmarkProfileDir = profileDir
			_, err = engine.db.ExecContext(
				context.Background(),
				"PRAGMA enable_profiling='json'",
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
			profilePaths, err := filepath.Glob(
				filepath.Join(profileDir, "statement-*.json"),
			)
			requirementsForTest.NoError(err)
			requirementsForTest.NotEmpty(profilePaths)
			profiles := make([]relationshipBenchmarkProfile, 0, len(profilePaths))
			for _, profilePath := range profilePaths {
				profileData, readErr := os.ReadFile(profilePath)
				requirementsForTest.NoError(readErr)
				var profile relationshipBenchmarkProfile
				requirementsForTest.NoError(json.Unmarshal(profileData, &profile))
				profiles = append(profiles, profile)
			}
			aggregated := aggregateRelationshipBenchmarkProfiles(profiles)
			rowsScanned += aggregated.RowsScanned
			spillBytes += max(aggregated.SpillBytes,
				relationshipBenchmarkDirectoryBytes(b, engine.tempDirectory))
			result.Statements = aggregated.Statements
			result.DatasetOperatorScans = aggregated.DatasetOperatorScans
			requirementsForTest.NoError(engine.Close())
			b.StartTimer()
		}
		b.ReportMetric(float64(rowsScanned)/float64(b.N), "rows-scanned/op")
		b.ReportMetric(float64(spillBytes)/float64(b.N), "spill-bytes/op")
		b.ReportMetric(float64(returnedRows)/float64(b.N), "rows-returned/op")
		result.RowsScanned = rowsScanned / int64(b.N)
		result.SpillBytes = spillBytes / int64(b.N)
		result.RowsReturned = returnedRows / int64(b.N)
	})
	return result
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
