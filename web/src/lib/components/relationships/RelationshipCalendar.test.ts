import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { RelationshipCalendar as RelationshipCalendarModel } from '../../relationships/models';
import RelationshipCalendar from './RelationshipCalendar.svelte';

function calendar(overrides: Partial<RelationshipCalendarModel> = {}): RelationshipCalendarModel {
  return {
    participant_id: 7,
    canonical_id: 7,
    year: 2026,
    timezone: 'UTC',
    days: [
      { date: '2026-01-01', sent: 0, received: 0, email: 0, chat: 0, meetings: 0, total: 0, modality_mask: 0, level: 'NONE' },
      { date: '2026-01-02', sent: 2, received: 1, email: 1, chat: 2, meetings: 0, total: 3, modality_mask: 3, level: 'FOURTH_QUARTILE' }
    ],
    current: {
      temperature: 62, rank: 4, population: 20, raw_score: 3,
      signals: { sent_signal: 1, received_volume: 1, meeting_signal: 0, modalities: 2 }
    },
    annual: [],
    peak_temperature: 87,
    peak_year: 2018,
    scoring_timezone: 'UTC',
    score_version: 1,
    effective_date: '2026-01-02',
    cache_revision: 'cache-1',
    identity_revision: 4,
    ...overrides
  };
}

describe('RelationshipCalendar', () => {
  it('renders the shared five-level calendar, accessible day facts, and aligned summary', () => {
    render(RelationshipCalendar, {
      calendar: calendar(), loading: false, error: null,
      firstYear: 2018, currentYear: 2026, onYearChange: vi.fn()
    });

    expect(screen.getByRole('heading', { name: 'Relationship' })).toBeTruthy();
    expect(screen.getByText('2026')).toBeTruthy();
    expect(screen.getByText('Current 62/100')).toBeTruthy();
    expect(screen.getByText('Peak 87/100 - 2018')).toBeTruthy();
    expect(screen.getByText('Less')).toBeTruthy();
    expect(screen.getByText('More')).toBeTruthy();
    expect(document.querySelectorAll('.legend .heat-cell')).toHaveLength(5);
    expect(document.querySelectorAll('.calendar-panel.full')).toHaveLength(1);
    expect(document.querySelectorAll('.calendar-panel.half')).toHaveLength(2);
    const fullPanel = document.querySelector<HTMLElement>('.calendar-panel.full');
    expect(fullPanel).not.toBeNull();
    expect(within(fullPanel!).getByRole('button', {
      name: '2026-01-02: 3 interactions; 2 sent, 1 received, 1 email, 2 chat, 0 meetings'
    }).classList.contains('level-fourth-quartile')).toBe(true);
    expect(document.querySelectorAll('.day.future button')).toHaveLength(0);
  });

  it('navigates within first/current year bounds with explicit accessible controls', async () => {
    const onYearChange = vi.fn();
    render(RelationshipCalendar, {
      calendar: calendar(), loading: false, error: null,
      firstYear: 2018, currentYear: 2026, onYearChange
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Previous relationship year' }));
    expect(onYearChange).toHaveBeenCalledWith(2025);
    expect((screen.getByRole('button', { name: 'Next relationship year' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('renders stable loading, failure, and no-interaction regions', async () => {
    const { rerender } = render(RelationshipCalendar, {
      calendar: null, loading: true, error: null,
      year: 2026, firstYear: 2018, currentYear: 2026, onYearChange: vi.fn()
    });
    expect(screen.getByText('Loading relationship activity…')).toBeTruthy();

    await rerender({
      calendar: null, loading: false, error: 'Analytical cache unavailable',
      year: 2026, firstYear: 2018, currentYear: 2026, onYearChange: vi.fn()
    });
    expect(screen.getByRole('alert').textContent).toContain('Analytical cache unavailable');
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();

    await rerender({
      calendar: calendar({ days: [] }), loading: false, error: null,
      firstYear: 2018, currentYear: 2026, onYearChange: vi.fn()
    });
    expect(screen.getByText('No interactions in 2026.')).toBeTruthy();
  });
});
