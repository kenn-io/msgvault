package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
)

var (
	ErrRelationshipPersonNotFound  = errors.New("relationship person not found")
	ErrInvalidRelationshipYear     = errors.New("invalid relationship year")
	ErrInvalidRelationshipTimezone = errors.New("invalid relationship timezone")
)

const relationshipScoringTimezone = "UTC"

// RelationshipCalendarAnalyzer is a dedicated cache-only capability. Live
// transactional query engines do not implement it and must not become an
// unbounded fallback.
type RelationshipCalendarAnalyzer interface {
	RelationshipCalendar(ctx context.Context, request RelationshipCalendarRequest) (*RelationshipCalendarResponse, error)
}

type RelationshipCanonicalResolver interface {
	ResolveCanonicalParticipant(ctx context.Context, participantID int64) (int64, error)
}

type RelationshipCalendarRequest struct {
	CanonicalID int64
	Year        int
	Timezone    string
}

type RelationshipCalendarDay struct {
	Date         string                  `json:"date"`
	Sent         int64                   `json:"sent"`
	Received     int64                   `json:"received"`
	Email        int64                   `json:"email"`
	Chat         int64                   `json:"chat"`
	Meetings     int64                   `json:"meetings"`
	Total        int64                   `json:"total"`
	ModalityMask uint8                   `json:"modality_mask"`
	Level        identityindex.HeatLevel `json:"level" enum:"NONE,FIRST_QUARTILE,SECOND_QUARTILE,THIRD_QUARTILE,FOURTH_QUARTILE"`
}

type RelationshipCalendarResponse struct {
	CanonicalID      int64                                    `json:"canonical_id"`
	Year             int                                      `json:"year"`
	Timezone         string                                   `json:"timezone"`
	Days             []RelationshipCalendarDay                `json:"days"`
	Current          identityindex.TemperatureSummary         `json:"current"`
	Annual           []identityindex.AnnualTemperatureSummary `json:"annual"`
	PeakTemperature  int                                      `json:"peak_temperature"`
	PeakYear         int                                      `json:"peak_year"`
	ScoringTimezone  string                                   `json:"scoring_timezone"`
	ScoreVersion     int                                      `json:"score_version"`
	EffectiveDate    string                                   `json:"effective_date"`
	CacheRevision    string                                   `json:"cache_revision"`
	IdentityRevision int64                                    `json:"identity_revision"`
}

type cachedAnnualTemperature struct {
	Year           int     `json:"year"`
	Temperature    int     `json:"temperature"`
	Rank           int64   `json:"rank"`
	Population     int64   `json:"population"`
	RawScore       float64 `json:"raw_score"`
	SentSignal     float64 `json:"sent_signal"`
	ReceivedVolume float64 `json:"received_volume"`
	MeetingSignal  float64 `json:"meeting_signal"`
	Modalities     int     `json:"modalities"`
}

func (e *DuckDBEngine) RelationshipCalendar(
	ctx context.Context,
	request RelationshipCalendarRequest,
) (*RelationshipCalendarResponse, error) {
	location, timezone, err := validateRelationshipCalendarRequest(request)
	if err != nil {
		return nil, err
	}
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	release, err := e.acquireCacheRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	response, effectiveAt, err := e.relationshipTemperatureSummary(ctx, request.CanonicalID)
	if err != nil {
		return nil, err
	}
	response.CanonicalID = request.CanonicalID
	response.Year = request.Year
	response.Timezone = timezone
	response.ScoringTimezone = relationshipScoringTimezone
	response.CacheRevision = state.Revision()
	response.IdentityRevision = state.IdentityRevision
	response.Days, err = e.relationshipCalendarDays(ctx, request, location, effectiveAt)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func validateRelationshipCalendarRequest(
	request RelationshipCalendarRequest,
) (*time.Location, string, error) {
	return validateRelationshipCalendarRequestAt(request, time.Now())
}

func validateRelationshipCalendarRequestAt(
	request RelationshipCalendarRequest,
	now time.Time,
) (*time.Location, string, error) {
	if request.CanonicalID < 1 {
		return nil, "", fmt.Errorf("%w: canonical participant ID must be positive", ErrInvalidExploreRequest)
	}
	timezone := request.Timezone
	if timezone == "" {
		timezone = relationshipScoringTimezone
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w: invalid timezone %q: %w",
			ErrInvalidExploreRequest, ErrInvalidRelationshipTimezone, timezone, err)
	}
	if request.Year < 1970 || request.Year > now.In(location).Year() {
		return nil, "", fmt.Errorf("%w: %w: relationship calendar year %d is outside the supported range",
			ErrInvalidExploreRequest, ErrInvalidRelationshipYear, request.Year)
	}
	return location, timezone, nil
}

func (e *DuckDBEngine) relationshipTemperatureSummary(
	ctx context.Context,
	canonicalID int64,
) (*RelationshipCalendarResponse, time.Time, error) {
	directory := quoteIdentitySQLPath(e.parquetPath(identityindex.DatasetPeople))
	var response RelationshipCalendarResponse
	var effectiveDate time.Time
	var annualJSON string
	err := e.db.QueryRowContext(ctx, `
		SELECT current_temperature, current_temperature_rank,
		       current_temperature_population, current_raw_score,
		       current_sent_signal, current_received_volume,
		       current_meeting_signal, current_modalities,
		       temperature_effective_at, temperature_score_version,
		       CAST(to_json(annual_temperatures) AS VARCHAR),
		       peak_temperature, peak_year
		FROM read_parquet('`+directory+`')
		WHERE canonical_id = ? AND NOT is_owner
	`, canonicalID).Scan(
		&response.Current.Temperature,
		&response.Current.Rank,
		&response.Current.Population,
		&response.Current.RawScore,
		&response.Current.Signals.SentSignal,
		&response.Current.Signals.ReceivedVolume,
		&response.Current.Signals.MeetingSignal,
		&response.Current.Signals.Modalities,
		&effectiveDate,
		&response.ScoreVersion,
		&annualJSON,
		&response.PeakTemperature,
		&response.PeakYear,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, time.Time{}, ErrRelationshipPersonNotFound
	case err != nil:
		return nil, time.Time{}, fmt.Errorf("read relationship temperature summary: %w", err)
	}
	var cached []cachedAnnualTemperature
	if err := json.Unmarshal([]byte(annualJSON), &cached); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode annual relationship temperatures: %w", err)
	}
	response.Annual = make([]identityindex.AnnualTemperatureSummary, len(cached))
	for i, annual := range cached {
		response.Annual[i] = identityindex.AnnualTemperatureSummary{
			Year:        annual.Year,
			Temperature: annual.Temperature,
			Rank:        annual.Rank,
			Population:  annual.Population,
			RawScore:    annual.RawScore,
			Signals: identityindex.TemperatureSignals{
				SentSignal: annual.SentSignal, ReceivedVolume: annual.ReceivedVolume,
				MeetingSignal: annual.MeetingSignal, Modalities: annual.Modalities,
			},
		}
	}
	response.EffectiveDate = effectiveDate.UTC().Format(time.DateOnly)
	return &response, effectiveDate, nil
}

func (e *DuckDBEngine) relationshipCalendarDays(
	ctx context.Context,
	request RelationshipCalendarRequest,
	location *time.Location,
	effectiveAt time.Time,
) ([]RelationshipCalendarDay, error) {
	startLocal := time.Date(request.Year, time.January, 1, 0, 0, 0, 0, location)
	endLocal := startLocal.AddDate(1, 0, 0)
	startUTC := startLocal.UTC()
	endUTC := endLocal.UTC()
	years := make([]string, 0, 2)
	for year := startUTC.Year(); year <= endUTC.Add(-time.Nanosecond).Year(); year++ {
		years = append(years, strconv.Itoa(year))
	}
	cutoff := effectiveAt.UTC()
	activity := fmt.Sprintf(`(
		SELECT * FROM read_parquet('%s', hive_partitioning=true, union_by_name=true)
		WHERE occurred_year IN (%s)
		  AND occurred_at >= TIMESTAMP '%s'
		  AND occurred_at < TIMESTAMP '%s'
		  AND (canonical_id = %d OR is_owner)
	)`,
		e.identityActivityPath(), strings.Join(years, ","),
		startUTC.Format("2006-01-02 15:04:05.999999999"),
		endUTC.Format("2006-01-02 15:04:05.999999999"),
		request.CanonicalID,
	)
	facts := identityindex.RelationshipTemperatureFactsSQL(activity, cutoff)
	rows, err := e.db.QueryContext(ctx, `
		WITH relationship_facts AS (
		`+facts+`
		)
		SELECT strftime(timezone(?, timezone('UTC', occurred_at)), '%Y-%m-%d') AS local_date,
		       count(*) FILTER (WHERE sent)::BIGINT AS sent_count,
		       count(*) FILTER (WHERE received)::BIGINT AS received_count,
		       count(*) FILTER (WHERE email)::BIGINT AS email_count,
		       count(*) FILTER (WHERE chat)::BIGINT AS chat_count,
		       count(*) FILTER (WHERE meeting)::BIGINT AS meeting_count,
		       count(*)::BIGINT AS total_count,
		       bit_or(modality_mask)::UTINYINT AS modality_mask
		FROM relationship_facts
		WHERE canonical_id = ?
		GROUP BY local_date
		ORDER BY local_date
	`, request.TimezoneOrUTC(), request.CanonicalID)
	if err != nil {
		return nil, fmt.Errorf("query relationship calendar: %w", err)
	}
	defer func() { _ = rows.Close() }()
	active := make(map[string]identityindex.DailyTemperatureSignals)
	for rows.Next() {
		var date string
		var day identityindex.DailyTemperatureSignals
		if err := rows.Scan(
			&date, &day.Sent, &day.Received, &day.Email, &day.Chat,
			&day.Meetings, &day.Total, &day.ModalityMask,
		); err != nil {
			return nil, fmt.Errorf("scan relationship calendar day: %w", err)
		}
		active[date] = day
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationship calendar days: %w", err)
	}

	localEffectiveAt := effectiveAt.In(location)
	elapsedEnd := endLocal
	switch {
	case localEffectiveAt.Before(startLocal):
		elapsedEnd = startLocal
	case localEffectiveAt.Before(endLocal):
		elapsedEnd = time.Date(localEffectiveAt.Year(), localEffectiveAt.Month(), localEffectiveAt.Day(),
			0, 0, 0, 0, location).AddDate(0, 0, 1)
	}
	days := make([]RelationshipCalendarDay, 0, int(elapsedEnd.Sub(startLocal).Hours()/24)+1)
	signals := make([]identityindex.DailyTemperatureSignals, 0, cap(days))
	for date := startLocal; date.Before(elapsedEnd); date = date.AddDate(0, 0, 1) {
		key := date.Format(time.DateOnly)
		day := active[key]
		days = append(days, RelationshipCalendarDay{
			Date: key, Sent: day.Sent, Received: day.Received,
			Email: day.Email, Chat: day.Chat, Meetings: day.Meetings,
			Total: day.Total, ModalityMask: day.ModalityMask,
		})
		signals = append(signals, day)
	}
	levels := identityindex.AssignRelationshipHeatLevels(signals)
	for i := range days {
		days[i].Level = levels[i]
	}
	return days, nil
}

func (r RelationshipCalendarRequest) TimezoneOrUTC() string {
	if r.Timezone == "" {
		return relationshipScoringTimezone
	}
	return r.Timezone
}
