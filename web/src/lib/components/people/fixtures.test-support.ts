import type { RelationshipCalendar } from '../../relationships/models';

export const emptyCalendar = (year: number): RelationshipCalendar => ({
  participant_id: 1, canonical_id: 1, year, timezone: 'UTC', days: [], annual: [],
  current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } },
  peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1,
  effective_date: '2026-07-19', cache_revision: 'cache-1', identity_revision: 1,
});
