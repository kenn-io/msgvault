import { emptyProfile } from '../../directory/profile-fixtures.test-support';
import { act, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { DirectoryController } from '../../directory/controller.svelte';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import PersonPage from './PersonPage.svelte';
import PersonAddresses from './PersonAddresses.svelte';

vi.mock('./PersonAddresses.svelte', { spy: true });

const when = '2026-07-19T10:00:00Z';
afterEach(() => { vi.unstubAllEnvs(); });

const addressPerson = { id: 7, revision: 1, display_name: 'Alice Example', participant_ids: [1], vcard_uid: 'example-7', created_at: when, updated_at: when };

async function renderAddressPage(profileResponse: Response | Promise<Response>, waitForProfile = true) {
  const person = addressPerson;
  vi.stubEnv('TZ', 'UTC');
  const summary = { profile: person, id: 1, display_label: 'Alice Example', identifiers: [
    { type: 'email', value: 'alice@example.com', participant_id: 1, is_primary: true, provenance: 'participant_identifiers' },
    { type: 'email', value: 'alice@example.org', participant_id: 1, is_primary: false, provenance: 'participant_identifiers' },
  ], activity_count: 9, file_count: 2, first_at: '2026-06-01T12:00:00Z', last_at: when, source_counts: [], cache_revision: 'cache-detail' };
  const target = 'cluster:1';
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/people/7') return Response.json(person, { headers: { ETag: '"person-7-r1"' } });
    if (path === '/api/v1/people/7/profile') return profileResponse;
    if (path === '/api/v1/people/7/attributes') return Response.json({ person_id: 7, attributes: [] });
    if (path === '/api/v1/people/7/contact-state') return Response.json({ person_id: 7, cadence_status: 'unknown', interaction_count: 0, computed_at: when, stale: false });
    if (path === '/api/v1/participants/1') return Response.json(summary);
    if (path.endsWith('/files/search')) return Response.json({ files: [], total_count: 0, cache_revision: 'cache-detail', search_provenance: {} });
    return Response.json({ rows: [], total_count: 0, cache_revision: 'cache-detail', search_provenance: {} });
  });
  const controller = new RelationshipsController(client, () => 'UTC');
  const directory = new DirectoryController(client);
  await controller.openTarget(target, {});
  const opening = directory.loadSelection(7);
  if (waitForProfile) await opening;
  const view = render(PersonPage, { props: { client, directory, controller, target, filesOpen: false, predicate: {}, onFilesToggle: vi.fn(), onFiltersChange: vi.fn(), onReconcilePersonMerge: context => controller.reconcilePersonMerge(context) } });
  return { view, controller, directory, opening };
}

it.each(['loaded', 'failed'])('gates linked addresses until the profile read settles, forwards values, and limits them to Details (%s)', async outcome => {
  const contactPoints = [{ person_id: 7, address_kind: 'email', original_value: 'alice@example.com', normalized_value: 'alice@example.com', normalization: 'email', normalization_version: 1, envelope: { id: 1, ordinal: 0, sensitive: false, source: 'user', created_at: when, updated_at: when, vcard: {} } }];
  let releaseProfile!: (response: Response) => void;
  const pendingProfile = new Promise<Response>(resolve => { releaseProfile = resolve; });
  const { view, controller, directory, opening } = await renderAddressPage(pendingProfile, false);
  try {
    await fireEvent.click(screen.getByRole('radio', { name: 'Details' }));
    expect(screen.queryByRole('heading', { name: 'Linked addresses' })).toBeNull();
    await act(async () => { releaseProfile(outcome === 'loaded' ? Response.json({ ...emptyProfile(addressPerson), contact_points: contactPoints }, { headers: { ETag: '"person-7-r1"' } }) : Response.json({ message: 'Profile unavailable' }, { status: 503 })); await opening; });
    if (outcome === 'loaded') expect(vi.mocked(PersonAddresses).mock.calls.at(-1)![1].listedInDetails).toEqual(contactPoints);
    else expect(screen.getByLabelText('Identity alice@example.com')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Linked addresses' }).closest('.details')).toHaveProperty('hidden', false);
    await fireEvent.click(screen.getByRole('radio', { name: 'Files' }));
    await view.rerender({ filesOpen: true });
    expect(screen.getByRole('heading', { name: 'Linked addresses', hidden: true }).closest('.details')).toHaveProperty('hidden', true);
    await fireEvent.click(screen.getByRole('radio', { name: 'Messages' }));
    await view.rerender({ filesOpen: false });
    expect(screen.getByRole('heading', { name: 'Linked addresses', hidden: true }).closest('.details')).toHaveProperty('hidden', true);
  } finally { view.unmount(); controller.destroy(); directory.destroy(); }
});
