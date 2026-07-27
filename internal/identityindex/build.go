package identityindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode selects which cache population a build reads and replaces.
type Mode uint8

const (
	ModeFull Mode = iota
	ModeIncremental
	ModeDerivedOnly
)

// BuildOptions identifies committed, staged, and output cache roots.
type BuildOptions struct {
	Mode           Mode
	CommittedRoot  string
	StagedBaseRoot string
	OutputRoot     string
	AnchorDate     time.Time
}

// BuildResult contains marker data derived alongside the index.
type BuildResult struct {
	ConversationParticipantsFingerprint string
	Stats                               CacheStatsSummary
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Build derives identity facts and indexes from schema-correct base Parquet.
func Build(ctx context.Context, db sqlExecutor, opts BuildOptions) (BuildResult, error) {
	if db == nil {
		return BuildResult{}, errors.New("build identity index: nil database")
	}
	if err := validateBuildOptions(opts); err != nil {
		return BuildResult{}, err
	}
	if opts.Mode > ModeDerivedOnly {
		return BuildResult{}, fmt.Errorf("build identity index: unknown mode %d", opts.Mode)
	}

	b := builder{db: db, opts: opts}
	if opts.Mode == ModeDerivedOnly {
		for _, dataset := range baseIdentityDatasets {
			if err := requireParquetDataset(opts.CommittedRoot, dataset); err != nil {
				return BuildResult{}, fmt.Errorf("build identity index in derived-only mode: %w", err)
			}
		}
		for _, dataset := range []string{DatasetEntryFacts, DatasetDirectEdges} {
			if err := requireParquetDataset(opts.CommittedRoot, dataset); err != nil {
				return BuildResult{}, fmt.Errorf("build identity index in derived-only mode: %w", err)
			}
		}
	} else {
		for _, dataset := range baseIdentityDatasets {
			if err := requireParquetDataset(opts.StagedBaseRoot, dataset); err != nil {
				return BuildResult{}, fmt.Errorf("build identity index in mode %d: %w", opts.Mode, err)
			}
		}
	}
	if opts.Mode == ModeIncremental {
		for _, dataset := range baseIdentityDatasets {
			if err := requireParquetDataset(opts.CommittedRoot, dataset); err != nil {
				return BuildResult{}, fmt.Errorf("build identity index in incremental mode: %w", err)
			}
		}
	}
	outputs := []string{
		DatasetDirectory,
		DatasetRollups,
		DatasetDomainRollups,
		DatasetRelationships,
		DatasetRelationshipFuture,
	}
	if opts.Mode != ModeDerivedOnly {
		outputs = append(outputs, DatasetEntryFacts, DatasetDirectEdges, DatasetConversationEdges)
	} else if datasetContainsParquet(opts.StagedBaseRoot, "conversation_participants") {
		outputs = append(outputs, DatasetConversationEdges)
	}
	for _, dataset := range outputs {
		if err := resetOutputDataset(opts.OutputRoot, dataset); err != nil {
			return BuildResult{}, err
		}
	}

	if opts.Mode != ModeDerivedOnly {
		if err := b.copyDataset(ctx, DatasetEntryFacts, buildEntryFactsSQL(b.base)); err != nil {
			return BuildResult{}, err
		}
		if err := b.copyDataset(ctx, DatasetDirectEdges, buildDirectEdgesSQL(
			b.base,
			b.output(DatasetEntryFacts),
		)); err != nil {
			return BuildResult{}, err
		}
	}
	if opts.Mode != ModeDerivedOnly ||
		datasetContainsParquet(opts.StagedBaseRoot, "conversation_participants") {
		if err := b.copyDataset(ctx, DatasetConversationEdges, buildConversationEdgesSQL(b.base)); err != nil {
			return BuildResult{}, err
		}
	}
	if err := b.copyDataset(ctx, DatasetDirectory, buildDirectorySQL(b.base)); err != nil {
		return BuildResult{}, err
	}
	activityPaths := b.activityPaths()
	if err := b.copyDataset(ctx, DatasetRollups, buildIdentityRollupsSQL(activityPaths)); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(ctx, DatasetDomainRollups, buildDomainRollupsSQL(activityPaths)); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(
		ctx,
		DatasetRelationships,
		buildRelationshipRollupsSQL(activityPaths, opts.AnchorDate),
	); err != nil {
		return BuildResult{}, err
	}
	if err := b.copyDataset(
		ctx,
		DatasetRelationshipFuture,
		buildRelationshipFutureSQL(activityPaths, opts.AnchorDate),
	); err != nil {
		return BuildResult{}, err
	}
	if err := Validate(ctx, db, ValidationOptions{
		OutputRoot:             opts.OutputRoot,
		RequiredOutputDatasets: outputs,
		Activity:               activityPaths,
		Participants:           b.base("participants"),
		Conversations:          b.base("conversations"),
		AnchorDate:             opts.AnchorDate,
	}); err != nil {
		return BuildResult{}, err
	}

	fingerprint, err := conversationParticipantsFingerprint(ctx, db, b.base("conversation_participants"))
	if err != nil {
		return BuildResult{}, err
	}
	stats, err := collectCacheStats(ctx, db, b.statsRelations())
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		ConversationParticipantsFingerprint: fingerprint,
		Stats:                               stats,
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
	if b.opts.Mode == ModeDerivedOnly &&
		!datasetContainsParquet(b.opts.StagedBaseRoot, dataset) {
		return b.committed(dataset)
	}
	return parquetDatasetGlob(b.opts.StagedBaseRoot, dataset)
}

func (b builder) output(dataset string) string {
	return parquetDatasetGlob(b.opts.OutputRoot, dataset)
}

func (b builder) committed(dataset string) string {
	return parquetDatasetGlob(b.opts.CommittedRoot, dataset)
}

func (b builder) activityPaths() ActivityPaths {
	facts := []string{b.output(DatasetEntryFacts)}
	directEdges := []string{b.output(DatasetDirectEdges)}
	switch b.opts.Mode {
	case ModeFull:
	case ModeIncremental:
		facts = append([]string{b.committed(DatasetEntryFacts)}, facts...)
		directEdges = append([]string{b.committed(DatasetDirectEdges)}, directEdges...)
	case ModeDerivedOnly:
		facts = []string{b.committed(DatasetEntryFacts)}
		directEdges = []string{b.committed(DatasetDirectEdges)}
	}
	conversationEdges := b.output(DatasetConversationEdges)
	if b.opts.Mode == ModeDerivedOnly &&
		!datasetContainsParquet(b.opts.StagedBaseRoot, "conversation_participants") {
		conversationEdges = b.committed(DatasetConversationEdges)
	}
	return ActivityPaths{
		Facts:             readParquetRelation(facts, true),
		DirectEdges:       readParquetRelation(directEdges, false),
		ConversationEdges: conversationEdges,
		Directory:         b.output(DatasetDirectory),
		Clusters:          b.base("participant_clusters"),
		Owners:            b.base("owner_participants"),
	}
}

func (b builder) statsRelations() cacheStatsRelations {
	inputs := cacheStatsRelations{
		messages:     readParquetRelation([]string{b.base("messages")}, true),
		recipients:   readParquetRelation([]string{b.base("message_recipients")}, false),
		participants: readParquetRelation([]string{b.base("participants")}, false),
		attachments:  readParquetRelation([]string{b.base("attachments")}, false),
	}
	if b.opts.Mode == ModeDerivedOnly {
		return cacheStatsRelations{
			messages:     readParquetRelation([]string{b.committed("messages")}, true),
			recipients:   readParquetRelation([]string{b.committed("message_recipients")}, false),
			participants: readParquetRelation([]string{b.committed("participants")}, false),
			attachments:  readParquetRelation([]string{b.committed("attachments")}, false),
		}
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
	output := filepath.Join(b.opts.OutputRoot, dataset, "data.parquet")
	copyOptions := "FORMAT PARQUET, COMPRESSION 'zstd'"
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		output = filepath.Join(b.opts.OutputRoot, dataset)
		copyOptions += ", PARTITION_BY (occurred_year), WRITE_PARTITION_COLUMNS true, OVERWRITE_OR_IGNORE"
	}
	statement := "COPY (" + query + ") TO '" + quoteSQLString(output) +
		"' (" + copyOptions + ")"
	if _, err := b.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("build %s: %w", dataset, err)
	}
	if (dataset == DatasetEntryFacts || dataset == DatasetDirectEdges) &&
		!datasetContainsParquet(b.opts.OutputRoot, dataset) {
		emptyOutput := filepath.Join(b.opts.OutputRoot, dataset, "empty.parquet")
		emptyStatement := "COPY (SELECT * FROM (" + query + ") WHERE false) TO '" +
			quoteSQLString(emptyOutput) + "' (FORMAT PARQUET, COMPRESSION 'zstd')"
		if _, err := b.db.ExecContext(ctx, emptyStatement); err != nil {
			return fmt.Errorf("build empty %s: %w", dataset, err)
		}
	}
	return nil
}

func validateBuildOptions(opts BuildOptions) error {
	for name, root := range map[string]string{
		"staged base": opts.StagedBaseRoot,
		"output":      opts.OutputRoot,
	} {
		if root == "" || !filepath.IsAbs(root) {
			return fmt.Errorf("build identity index: %s root must be absolute", name)
		}
	}
	if opts.Mode != ModeFull {
		if opts.CommittedRoot == "" || !filepath.IsAbs(opts.CommittedRoot) {
			return errors.New("build identity index: committed root must be absolute outside full mode")
		}
	}
	if opts.AnchorDate.IsZero() {
		return errors.New("build identity index: relationship anchor date is required")
	}
	return nil
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
	err := filepath.WalkDir(datasetRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
			found = true
		}
		return nil
	})
	if err != nil {
		return false
	}
	return found
}

func resetOutputDataset(root, dataset string) error {
	path := filepath.Join(root, dataset)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("reset identity index dataset %s: %w", dataset, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create identity index dataset %s: %w", dataset, err)
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

	hash := sha256.New()
	encoded := make([]byte, 0, 48)
	for rows.Next() {
		var conversationID, participantID int64
		if err := rows.Scan(&conversationID, &participantID); err != nil {
			return "", fmt.Errorf("scan conversation participant fingerprint: %w", err)
		}
		encoded = strconv.AppendInt(encoded[:0], conversationID, 10)
		encoded = append(encoded, ':')
		encoded = strconv.AppendInt(encoded, participantID, 10)
		encoded = append(encoded, '\n')
		_, _ = hash.Write(encoded)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate conversation participant fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
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
			(SELECT coalesce(sum(try_cast(size AS BIGINT)), 0) FROM %s)
	`,
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
		return CacheStatsSummary{}, fmt.Errorf("collect identity cache stats: %w", err)
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
	if dataset == "messages" ||
		dataset == DatasetEntryFacts ||
		dataset == DatasetDirectEdges {
		return filepath.Join(root, dataset, "**", "*.parquet")
	}
	return filepath.Join(root, dataset, "*.parquet")
}

func quoteSQLString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
