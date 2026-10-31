import { render, screen } from '@testing-library/svelte';
import { it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import type { RelationshipCalendar } from '../../relationships/models';
import { meetingFixtureResponse } from '../../meetings/fixtures.test-support';
import PersonPage from './PersonPage.svelte';

const when = '2026-07-19T10:00:00Z';

it('refreshes the open person after linking and profile reconciliation', async () => {
  let linked = false;
  const routes: Record<string, (request: Request) => Response | Promise<Response>> = {
    '/api/v1/participants/1': () => Response.json({ id: 1, display_label: 'Alice Example', identifiers: [], first_at: '2025-01-01T00:00:00Z', last_at: when, activity_count: 2, file_count: 0, source_counts: [] }),
    '/api/v1/relationships/1/calendar': async request => Response.json({ participant_id: 1, canonical_id: 1, year: (await request.json()).year, timezone: 'UTC', current: { temperature: 50, rank: 1, population: 1, raw_score: 1, signals: { sent_signal: 1, received_volume: 0, meeting_signal: 0, modalities: 1 } }, peak_temperature: 50, peak_year: 2026, scoring_timezone: 'UTC', score_version: 1, effective_date: '2026-07-19', days: [], annual: [], identity_revision: linked ? 2 : 1, cache_revision: 'cache-rel' } satisfies RelationshipCalendar),
    '/api/v1/identity/links': () => { linked = true; return Response.json({ identity_revision: 2, cache_state: 'ready' }); },
    '/api/v1/participants/1/timeline': () => Response.json({ rows: [{ key: 'message:1', kind: 'email', match: {}, title: linked ? 'Linked message' : 'Original message', occurred_at: when, message_count: 1, participant_labels: [], source_type: 'gmail', source_id: 1 }], total_count: 1, cache_revision: 'cache-rel', search_provenance: {} })
  };
  const client = createAPIClient(async input => { const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname; return routes[path]?.(request) ?? meetingFixtureResponse(path) ?? Response.json({ rows: [] }); });
  const controller = new RelationshipsController(client, () => 'UTC'); const predicate = { filters: [], presentation: 'table' as const };
  await controller.openTarget('cluster:1', predicate, false);
  const view = render(PersonPage, { props: { client, controller, target: 'cluster:1', filesOpen: false, predicate, onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await screen.findByText('Original message');
    await controller.linkParticipants(1, 2);
    await screen.findByText('Linked message');
    linked = false;
    await controller.reconcilePersonMerge(controller.personMergeContextSnapshot());
    await screen.findByText('Original message');
  } finally { view.unmount(); controller.destroy(); }
});
