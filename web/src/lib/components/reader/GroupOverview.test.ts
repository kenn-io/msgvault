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
