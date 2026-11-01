import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { PendingUndo } from '../../people/pending-undo.svelte';
import PeopleCatalogSettings from './PeopleCatalogSettings.svelte';
import PersonFields from '../people/PersonFields.svelte';

it('makes a custom field created in Settings available in More fields', async () => {
  let created = false;
  const writes: Request[] = [];
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1 };
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (request.method === 'POST') { writes.push(request); created = true; return Response.json(definition, { status: 201 }); }
      return Response.json({ definitions: created ? [definition] : [] });
    }
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [] });
    throw new Error(`Unexpected ${path}`);
  });
  const settings = render(PeopleCatalogSettings, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Add field' }));
  await fireEvent.input(screen.getByRole('textbox', { name: 'Label' }), { target: { value: 'Favorite color' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
  await screen.findByText('Created Favorite color');
  expect(writes).toHaveLength(1);
  await expect(writes[0]!.json()).resolves.toMatchObject({ label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single' });
  await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
  expect(screen.getByRole('button', { name: 'Edit Favorite color' })).toBeDefined();
  settings.unmount();
  const controller = new DirectoryProfileController(client, 7, { etags: {}, errors: {} });
  const fields = render(PersonFields, { controller, pending: new PendingUndo() });
  await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
  await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Favorite color' })).toBeDefined());
  fields.unmount(); controller.destroy();
});
