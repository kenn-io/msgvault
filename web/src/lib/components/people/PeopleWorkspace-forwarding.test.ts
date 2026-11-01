import { render, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { RelationshipsController } from '../../relationships/controller.svelte';
import { DirectoryController } from '../../directory/controller.svelte';
import { ExploreState } from '../../explore/state.svelte';
import PeopleWorkspace from './PeopleWorkspace.svelte';
import PersonPage from './PersonPage.svelte';

vi.mock('./PersonPage.svelte', { spy: true });

it('forwards disabled sweep configuration and settings navigation to the person page', async () => {
  const sweepConfigured = false;
  vi.mocked(PersonPage).mockClear();
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    const person = { id: 7, revision: 1, display_name: 'Synthetic One', participant_ids: [] };
    if (path === '/api/v1/people/7') return Response.json(person);
    if (path.endsWith('/profile')) return Response.json({ person, names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
    return Response.json({ rows: [] });
  });
  const state = new ExploreState(null); state.commitNavigation({ workspace: 'relationships', directoryPersonID: 7 });
  const directory = new DirectoryController(client); const controller = new RelationshipsController(client, () => 'UTC');
  const onOpenSettings = vi.fn();
  const view = render(PeopleWorkspace, { props: { client, controller, directory, sweepConfigured, onOpenSettings, urlState: state.current, target: null, filesOpen: false, predicate: {}, onNavigate: vi.fn(), onReplaceNavigation: vi.fn(), onDraftNavigation: vi.fn(), onShowAllChange: vi.fn(), onFilesToggle: vi.fn(), onFiltersChange: vi.fn() } });
  try {
    await waitFor(() => expect(PersonPage).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ sweepConfigured, onOpenSettings })));
  } finally { view.unmount(); directory.destroy(); controller.destroy(); state.destroy(); }
});
