import { render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import GroupOverview from './GroupOverview.svelte';

it('shows the complete range, daily strip, and people from a month overview', async () => {
 const selection = { kind: 'group' as const, dimension: 'month' as const, key: '2024-05', label: 'May 2024', count: 3, latestAt: '2024-05-31T12:00:00Z', overview: { first_at: '2024-05-03T12:00:00Z', activity: [{ bucket: '2024-05-03', count: 3 }], co_participants: [{ key: '1', label: 'Person Example', count: 2 }] } };
 const view = render(GroupOverview, { selection });
 expect(screen.getByText('May 3, 2024 – May 31, 2024')).toBeDefined();
 expect(screen.getByRole('img', { name: /Activity by day/ }).querySelectorAll('rect')).toHaveLength(1);
 expect(screen.getByText('Most often with')).toBeDefined();
 expect(screen.getByText('Person Example')).toBeDefined();
 await view.rerender({ selection: { ...selection, overview: { ...selection.overview, co_participants: [] } } });
 expect(screen.queryByText('Most often with')).toBeNull();
});

it.each([
  ['2024-05-03', 'day', 'May 3, 2024', 1],
  ['2024-05', 'month', 'May 2024', 1],
])('shows one date and a concise summary for a single %s bucket', (bucket, unit, label, visibleLabels) => {
  render(GroupOverview, { selection: {
    kind: 'group', dimension: 'month', key: '2024-05', label: 'May 2024', count: 3,
    latestAt: '2024-05-03T12:00:00',
    overview: { first_at: '2024-05-03T08:00:00', activity: [{ bucket, count: 3 }], co_participants: [] },
  } });

  expect(screen.getByRole('img', { name: `Activity by ${unit}: ${label}, 3 items` })).toBeDefined();
  expect(screen.getAllByText('May 3, 2024')).toHaveLength(1);
  if (unit === 'month') expect(screen.getAllByText(label)).toHaveLength(visibleLabels);
});

it('keeps both endpoints and the busiest date for multiple buckets', () => {
  render(GroupOverview, { selection: {
    kind: 'group', dimension: 'month', key: '2024-05', label: 'May 2024', count: 4,
    overview: { first_at: '2024-05-03T08:00:00', activity: [{ bucket: '2024-05-03', count: 3 }, { bucket: '2024-05-05', count: 1 }], co_participants: [] },
  } });

  expect(screen.getByText('May 3, 2024')).toBeDefined();
  expect(screen.getByText('May 5, 2024')).toBeDefined();
  expect(screen.getByRole('img', { name: 'Activity by day, May 3, 2024 to May 5, 2024, busiest May 3, 2024 with 3 items' })).toBeDefined();
});
