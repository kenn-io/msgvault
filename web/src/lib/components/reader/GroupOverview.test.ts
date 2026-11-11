import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import GroupOverview from './GroupOverview.svelte';

describe('GroupOverview', () => {
  it.each([
    { name: 'daily', selection: {
      kind: 'group' as const,
      dimension: 'month' as const,
      key: '2024-05',
      label: 'May 2024',
      count: 4,
      latestAt: '2024-05-31T12:00:00Z',
      overview: {
        first_at: '2024-05-03T12:00:00Z',
        activity: [{ bucket: '2024-05-03', count: 3 }, { bucket: '2024-05-31', count: 1 }],
        co_participants: [{ key: '1', label: 'Person Example', count: 2 }]
      }
    }, range: 'May 3, 2024 – May 31, 2024', showRange: false,
    image: 'Activity by day, May 3, 2024 to May 31, 2024, busiest May 3, 2024 with 3 items', dates: ['May 3, 2024', 'May 31, 2024'] },
    { name: 'monthly', selection: {
      kind: 'group' as const, dimension: 'participant' as const, key: '42', label: 'Person Example', count: 3, latestAt: '2024-06-20T12:00:00Z',
      overview: { first_at: '2024-05-03T12:00:00Z', activity: [{ bucket: '2024-05', count: 2 }, { bucket: '2024-06', count: 1 }], co_participants: [{ key: '1', label: 'Person Example', count: 2 }] }
    }, range: 'May 3, 2024 – Jun 20, 2024', showRange: true,
    image: 'Activity by month, May 2024 to Jun 2024, busiest May 2024 with 2 items', dates: [] }
  ])('shows the range, $name strip, and people', async ({ selection, range, showRange, image, dates }) => {
    const view = render(GroupOverview, { selection });

    if (showRange) expect(screen.getByText(range)).toBeDefined();
    else expect(screen.queryByText(range)).toBeNull();
    expect(screen.getByRole('img', { name: image })).toBeDefined();
    for (const date of dates) expect(screen.getByText(date)).toBeDefined();
    expect(screen.getByText('Most often with')).toBeDefined();
    expect(screen.getByText('Person Example')).toBeDefined();

    await view.rerender({ selection: { ...selection, overview: { ...selection.overview, co_participants: [] } } });
    expect(screen.queryByText('Most often with')).toBeNull();
  });

  it.each([
    ['2024-05-03', 'day', 'May 3, 2024', ['May 3, 2024']],
    ['2024-05', 'month', 'May 2024', ['May 3, 2024', 'May 2024']]
  ])('shows one date and a concise summary for a single %s bucket', (bucket, unit, label, visibleText) => {
    render(GroupOverview, { selection: {
      kind: 'group',
      dimension: unit === 'day' ? 'month' : 'participant',
      key: unit === 'day' ? '2024-05' : '42',
      label: unit === 'day' ? 'May 2024' : 'Person Example',
      count: 3,
      latestAt: '2024-05-03T12:00:00Z',
      overview: { first_at: '2024-05-03T12:00:00Z', activity: [{ bucket, count: 3 }], co_participants: [] }
    } });

    expect(screen.getByRole('img', { name: `Activity by ${unit}: ${label}, 3 items` })).toBeDefined();
    for (const text of visibleText) expect(screen.getAllByText(text)).toHaveLength(1);
  });
});
