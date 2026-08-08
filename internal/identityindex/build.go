package identityindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode selects the base population used to build the relationship index.
type Mode uint8

const (
	ModeFull Mode = iota
	ModeIncremental
	ModeIndexOnly
)

// BuildOptions identifies committed, staged, and output cache roots.
type BuildOptions struct {
	Mode           Mode
	CommittedRoot  string
	StagedBaseRoot string
	OutputRoot     string
	Progress       func(dataset string, elapsed time.Duration)
}

// ActivityStats records the fan-out chosen by the flat activity grain.
type ActivityStats struct {
	DirectRows               int64
	ConversationExpandedRows int64
	FinalRows                int64
	ExpansionRatio           float64
}

// BuildResult contains marker data derived alongside the index.
type BuildResult struct {
	ConversationParticipantsFingerprint string
	Stats                               CacheStatsSummary
	Activity                            ActivityStats
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Build derives the four relationship datasets from schema-correct base
// Parquet. Incremental mode writes an activity delta and rebuilds the compact
// datasets over the committed population plus that delta.
func Build(
	ctx context.Context,
	db sqlExecutor,
	opts BuildOptions,
) (result BuildResult, resultErr error) {
	if db == nil {
		return BuildResult{}, errors.New("build relationship index: nil database")
	}
	if err := validateBuildOptions(opts); err != nil {
		return BuildResult{}, err
	}
	if opts.Mode > ModeIndexOnly {
		return BuildResult{}, fmt.Errorf("build relationship index: unknown mode %d", opts.Mode)
	}

	b := builder{db: db, opts: opts}
	defer func() {
		resultErr = errors.Join(resultErr, b.dropBuildTables())
	}()

	for _, dataset := range baseIdentityDatasets {
		root := opts.StagedBaseRoot
		if opts.Mode == ModeIndexOnly &&
			!datasetContainsParquet(opts.StagedBaseRoot, dataset) {
			root = opts.CommittedRoot
		}
		if err := requireParquetDataset(root, dataset); err != nil {
			return BuildResult{}, fmt.Errorf("build relationship index in mode %d: %w", opts.Mode, err)
		}
	}
	if opts.Mode == ModeIncremental {
		if err := requireParquetDataset(opts.CommittedRoot, DatasetActivity); err != nil {
			return BuildResult{}, fmt.Errorf("build incremental relationship index: %w", err)
		}
	}
	for _, dataset := range RequiredDatasets {
		if err := resetOutputDataset(opts.OutputRoot, dataset); err != nil {
			return BuildResult{}, err
		}
	}

	if err := b.copyRelationshipActivity(ctx); err != nil {
		return BuildResult{}, err
	}
	activity := b.activityRelation()
	if err := b.materializeBuildTable(
		ctx,
		directoryBuildRelation,
		buildDirectorySQL(b.base),
		"relationship_directory",
	); err != nil {
		return BuildResult{}, err
	}
	if err := b.materializeBuildTable(
		ctx,
		logicalBuildRelation,
		buildLogicalActivityMaterializationSQL(activity),
		"logical_activity",
	); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(ctx, DatasetPeople, buildRelationshipPeopleSQL()); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(ctx, DatasetDomains, buildRelationshipDomainsSQL()); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(ctx, DatasetRelationshipDaily, buildRelationshipDailySQL()); err != nil {
		return BuildResult{}, err
	}
	if err := Validate(ctx, db, ValidationOptions{
		OutputRoot:             opts.OutputRoot,
		RequiredOutputDatasets: RequiredDatasets,
		ActivityPath:           activity,
	}); err != nil {
		return BuildResult{}, err
	}

	fingerprint, err := conversationParticipantsFingerprint(
		ctx,
		db,
		b.base("conversation_participants"),
	)
	if err != nil {
		return BuildResult{}, err
	}
	activityStats, err := collectActivityStats(ctx, db, activity)
	if err != nil {
		return BuildResult{}, err
	}
	var stats CacheStatsSummary
	if opts.Mode != ModeIndexOnly {
		stats, err = collectCacheStats(ctx, db, b.statsRelations())
		if err != nil {
			return BuildResult{}, err
		}
	}
	return BuildResult{
		ConversationParticipantsFingerprint: fingerprint,
		Stats:                               stats,
		Activity:                            activityStats,
	}, nil
}

var baseIdentityDatasets = []string{
	"messages",
	"sources",
	"conversations",
	"participants",
	"participant_identifiers",
	"message_recipients",
	"conversation_participants",
	"owner_participants",
	"participant_clusters",
	"attachments",
}

type builder struct {
	db   sqlExecutor
	opts BuildOptions
}

func (b builder) base(dataset string) string {
	if b.opts.Mode == ModeIndexOnly &&
		!datasetContainsParquet(b.opts.StagedBaseRoot, dataset) {
		return b.committed(dataset)
	}
	return parquetDatasetGlob(b.opts.StagedBaseRoot, dataset)
}

func (b builder) committed(dataset string) string {
	return parquetDatasetGlob(b.opts.CommittedRoot, dataset)
}

func (b builder) output(dataset string) string {
	return parquetDatasetGlob(b.opts.OutputRoot, dataset)
}

func (b builder) activityRelation() string {
	paths := []string{b.output(DatasetActivity)}
	if b.opts.Mode == ModeIncremental {
		paths = append([]string{b.committed(DatasetActivity)}, paths...)
	}
	return readParquetRelation(paths, true)
}

func (b builder) statsRelations() cacheStatsRelations {
	inputs := cacheStatsRelations{
		messages:     readParquetRelation([]string{b.base("messages")}, true),
		recipients:   readParquetRelation([]string{b.base("message_recipients")}, false),
		participants: readParquetRelation([]string{b.base("participants")}, false),
		attachments:  readParquetRelation([]string{b.base("attachments")}, false),
	}
	if b.opts.Mode == ModeIncremental {
		inputs.messages = readParquetRelation(
			[]string{b.committed("messages"), b.base("messages")},
			true,
		)
		inputs.recipients = readParquetRelation(
			[]string{b.committed("message_recipients"), b.base("message_recipients")},
			false,
		)
		inputs.attachments = readParquetRelation(
			[]string{b.committed("attachments"), b.base("attachments")},
			false,
		)
	}
	return inputs
}

func (b builder) copyDataset(ctx context.Context, dataset, query string) error {
	start := time.Now()
	output := filepath.Join(b.opts.OutputRoot, dataset, "data.parquet")
	options := "FORMAT PARQUET, COMPRESSION 'zstd'"
	statement := "COPY (" + query + ") TO '" + quoteSQLString(output) +
		"' (" + options + ")"
	if _, err := b.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("build %s: %w", dataset, err)
	}
	if b.opts.Progress != nil {
		b.opts.Progress(dataset, time.Since(start))
	}
	return nil
}

func (b builder) copyRelationshipActivity(ctx context.Context) error {
	start := time.Now()
	years, err := b.relationshipActivityYears(ctx)
	if err != nil {
		return err
	}

	output := filepath.Join(b.opts.OutputRoot, DatasetActivity)
	options := "FORMAT PARQUET, COMPRESSION 'zstd', PARTITION_BY (occurred_year), " +
		"WRITE_PARTITION_COLUMNS true, OVERWRITE_OR_IGNORE"
	for _, year := range years {
		query := buildRelationshipActivitySQL(b.base, year)
		statement := "COPY (" + query + ") TO '" + quoteSQLString(output) +
			"' (" + options + ")"
		if _, err := b.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("build %s for year %d: %w", DatasetActivity, year, err)
		}
	}

	if !datasetContainsParquet(b.opts.OutputRoot, DatasetActivity) {
		if err := b.copyEmptyRelationshipActivity(ctx); err != nil {
			return err
		}
	}
	if b.opts.Progress != nil {
		b.opts.Progress(DatasetActivity, time.Since(start))
	}
	return nil
}

func (b builder) relationshipActivityYears(ctx context.Context) ([]int64, error) {
	query := `
		SELECT DISTINCT year(sent_at)::BIGINT AS occurred_year
		FROM read_parquet('` + quoteSQLString(b.base("messages")) + `',
		                  hive_partitioning=true, union_by_name=true)
		ORDER BY occurred_year`
	rows, err := b.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list relationship activity years: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var years []int64
	for rows.Next() {
		var year int64
		if err := rows.Scan(&year); err != nil {
			return nil, fmt.Errorf("scan relationship activity year: %w", err)
		}
		years = append(years, year)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list relationship activity years: %w", err)
	}
	return years, nil
}

func (b builder) copyEmptyRelationshipActivity(ctx context.Context) error {
	emptyOutput := filepath.Join(
		b.opts.OutputRoot,
		DatasetActivity,
		"occurred_year=0",
		"empty.parquet",
	)
	if err := os.MkdirAll(filepath.Dir(emptyOutput), 0o755); err != nil {
		return fmt.Errorf("create empty %s partition: %w", DatasetActivity, err)
	}
	query := buildRelationshipActivitySQL(b.base, 0)
	statement := "COPY (SELECT * FROM (" + query + ") WHERE false) TO '" +
		quoteSQLString(emptyOutput) + "' (FORMAT PARQUET, COMPRESSION 'zstd')"
	if _, err := b.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("build empty %s: %w", DatasetActivity, err)
	}
	return nil
}

func (b builder) materializeBuildTable(
	ctx context.Context,
	name, query, progressName string,
) error {
	start := time.Now()
	if _, err := b.db.ExecContext(ctx, "CREATE TEMP TABLE "+name+" AS "+query); err != nil {
		return fmt.Errorf("materialize %s: %w", progressName, err)
	}
	if b.opts.Progress != nil {
		b.opts.Progress(progressName, time.Since(start))
	}
	return nil
}

func (b builder) dropBuildTables() error {
	var result error
	for _, table := range []string{logicalBuildRelation, directoryBuildRelation} {
		if _, err := b.db.ExecContext(
			context.Background(),
			"DROP TABLE IF EXISTS "+table,
		); err != nil {
			result = errors.Join(result, fmt.Errorf("drop %s: %w", table, err))
		}
	}
	return result
}

func validateBuildOptions(opts BuildOptions) error {
	for name, root := range map[string]string{
		"staged base": opts.StagedBaseRoot,
		"output":      opts.OutputRoot,
	} {
		if root == "" || !filepath.IsAbs(root) {
			return fmt.Errorf("build relationship index: %s root must be absolute", name)
		}
	}
	if opts.Mode != ModeFull {
		if opts.CommittedRoot == "" || !filepath.IsAbs(opts.CommittedRoot) {
			return errors.New("build relationship index: committed root must be absolute outside full mode")
		}
	}
	return nil
}

func collectActivityStats(
	ctx context.Context,
	db sqlExecutor,
	activity string,
) (ActivityStats, error) {
	var result ActivityStats
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE is_direct),
		       count(*) FILTER (WHERE is_conversation_member),
		       count(*)
		FROM `+activity).Scan(
		&result.DirectRows,
		&result.ConversationExpandedRows,
		&result.FinalRows,
	)
	if err != nil {
		return ActivityStats{}, fmt.Errorf("collect relationship activity stats: %w", err)
	}
	if result.DirectRows > 0 {
		result.ExpansionRatio = float64(result.FinalRows) / float64(result.DirectRows)
	}
	return result, nil
}

func requireParquetDataset(root, dataset string) error {
	if !datasetContainsParquet(root, dataset) {
		return fmt.Errorf("%s has no Parquet shard", dataset)
	}
	return nil
}

func datasetContainsParquet(root, dataset string) bool {
	datasetRoot := filepath.Join(root, dataset)
	found := false
	err := filepath.WalkDir(datasetRoot, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
			found = true
		}
		return nil
	})
	return err == nil && found
}

func resetOutputDataset(root, dataset string) error {
	path := filepath.Join(root, dataset)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("reset relationship index dataset %s: %w", dataset, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create relationship index dataset %s: %w", dataset, err)
	}
	return nil
}

func conversationParticipantsFingerprint(
	ctx context.Context,
	db sqlExecutor,
	path string,
) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT conversation_id::BIGINT, participant_id::BIGINT
		FROM read_parquet(?)
		ORDER BY conversation_id, participant_id
	`, path)
	if err != nil {
		return "", fmt.Errorf("fingerprint conversation participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	fingerprint, err := FingerprintConversationParticipants(rows)
	if rowsErr := rows.Err(); rowsErr != nil && err == nil {
		return "", fmt.Errorf("iterate conversation participant fingerprint rows: %w", rowsErr)
	}
	return fingerprint, err
}

func collectCacheStats(
	ctx context.Context,
	db sqlExecutor,
	inputs cacheStatsRelations,
) (CacheStatsSummary, error) {
	query := fmt.Sprintf(`
WITH messages AS (
	SELECT id::BIGINT AS id, source_id::BIGINT AS source_id,
	       sent_at::TIMESTAMP AS sent_at,
	       coalesce(try_cast(size_estimate AS BIGINT), 0) AS size_estimate
	FROM %s
), senders AS (
	SELECT DISTINCT p.email_address, p.domain
	FROM %s mr
	JOIN %s p ON p.id = try_cast(mr.participant_id AS BIGINT)
	WHERE mr.recipient_type = 'from'
)
SELECT
	(SELECT count(*) FROM messages),
	(SELECT count(DISTINCT source_id) FROM messages),
	(SELECT count(DISTINCT email_address) FROM senders),
	(SELECT count(DISTINCT domain) FROM senders),
	(SELECT min(year(sent_at)) FROM messages),
	(SELECT max(year(sent_at)) FROM messages),
	(SELECT coalesce(sum(size_estimate), 0) FROM messages),
	(SELECT coalesce(sum(try_cast(size AS BIGINT)), 0) FROM %s)`,
		inputs.messages,
		inputs.recipients,
		inputs.participants,
		inputs.attachments,
	)
	var result CacheStatsSummary
	var minYear, maxYear sql.NullInt64
	err := db.QueryRowContext(ctx, query).Scan(
		&result.TotalMessages,
		&result.Sources,
		&result.UniqueSenders,
		&result.UniqueDomains,
		&minYear,
		&maxYear,
		&result.TotalSizeBytes,
		&result.AttachmentSizeBytes,
	)
	if err != nil {
		return CacheStatsSummary{}, fmt.Errorf("collect relationship cache stats: %w", err)
	}
	if minYear.Valid {
		result.MinYear = &minYear.Int64
	}
	if maxYear.Valid {
		result.MaxYear = &maxYear.Int64
	}
	return result, nil
}

type cacheStatsRelations struct {
	messages     string
	recipients   string
	participants string
	attachments  string
}

func readParquetRelation(paths []string, hivePartitioning bool) string {
	quotedPaths := make([]string, len(paths))
	for i, path := range paths {
		quotedPaths[i] = "'" + quoteSQLString(path) + "'"
	}
	options := ""
	if hivePartitioning {
		options = ", hive_partitioning=true, union_by_name=true"
	}
	return "read_parquet([" + strings.Join(quotedPaths, ",") + "]" + options + ")"
}

func parquetDatasetGlob(root, dataset string) string {
	if dataset == "messages" || dataset == DatasetActivity {
		return filepath.Join(root, dataset, "**", "*.parquet")
	}
	return filepath.Join(root, dataset, "*.parquet")
}

func quoteSQLString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
