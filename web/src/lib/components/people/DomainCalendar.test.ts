import { render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import { RelationshipsController } from '../../relationships/controller.svelte';
import PersonPage from './PersonPage.svelte';

it('requests the last active local year once on initial domain open', async () => {
  const years: number[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/explore/groups') {
      const body = await request.json();
      years.push(new Date(body.filters.find((filter: { dimension: string }) => filter.dimension === 'after').values[0]).getFullYear());
    }
    return meetingFixtureResponse(path) ?? Response.json({ rows: [], total_count: 0, cache_revision: 'cache-domain', search_provenance: {} });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  controller.target = 'domain:example.com';
  controller.detail = { domain: 'example.com', cache_revision: 'cache-domain', activity_count: 1, person_count: 1, file_count: 0, source_counts: [], first_at: '2022-01-01T12:00:00Z', last_at: '2025-07-18T12:00:00Z' };
  controller.firstContactAt = '2022-01-01T12:00:00Z'; controller.lastContactAt = '2025-07-18T12:00:00Z';
  const view = render(PersonPage, { props: { client, controller, target: controller.target, filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('2025', { exact: true });
    await waitFor(() => expect(years).toEqual([2025]));
  } finally { view.unmount(); controller.destroy(); }
});
