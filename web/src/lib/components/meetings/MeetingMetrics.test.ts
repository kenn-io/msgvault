import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { meetingMetrics } from '../../meetings/fixtures.test-support';
import MeetingMetrics from './MeetingMetrics.svelte';

describe('MeetingMetrics', () => {
  it('shows the 6000-second fixture, duration coverage, evidence bases, and monthly rows', () => {
    render(MeetingMetrics, { metrics: meetingMetrics() });
    expect(screen.getByText('4 meetings')).toBeDefined();
    expect(screen.getByText('3 known · 1 unknown duration')).toBeDefined();
    expect(screen.getByLabelText('Total known meeting time').textContent).toContain('1h 40m');
    expect(screen.getByLabelText('Average known duration').textContent).toContain('33m 20s');
    expect(screen.queryByText(/recorded hours/i)).toBeNull();
    const bases = screen.getByRole('table', { name: 'Duration evidence' });
    expect(within(bases).getByRole('row', { name: /Scheduled.*1.*1h/ })).toBeDefined();
    expect(within(bases).getByRole('row', { name: /Transcript span.*1.*10m/ })).toBeDefined();
    const months = screen.getByRole('table', { name: 'Monthly meeting activity' });
    expect(within(months).getByRole('row', { name: /2026-01.*2.*2.*0.*1h 30m.*45m/ })).toBeDefined();
    expect(within(months).getByRole('row', { name: /2026-02.*2.*1.*1.*10m.*10m/ })).toBeDefined();
  });

  it.each([0, 2])('keeps average unavailable for %s meetings without duration evidence', (count) => {
    render(MeetingMetrics, { metrics: meetingMetrics({ totals: { meeting_count: count, known_duration_count: 0,
      unknown_duration_count: count, total_known_seconds: 0, average_known_seconds: null }, duration_by_basis: [], months: [] }) });
    expect(screen.getByText(`${count} meetings`)).toBeDefined();
    expect(screen.getByLabelText('Average known duration').textContent).toContain('Unavailable');
  });
});
