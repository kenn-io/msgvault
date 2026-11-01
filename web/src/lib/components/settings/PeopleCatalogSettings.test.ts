import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PeopleCatalogSettings from './PeopleCatalogSettings.svelte';
import PersonFields from '../people/PersonFields.svelte';

it('creates a field for More fields and retains catalog edit, deactivate, conflict recovery, reactivate, and delete', async () => {
  let created = false; let conflict = true; const reads: URL[] = [];
  const writes: Request[] = [];
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true };
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (request.method === 'POST') { writes.push(request); created = true; return Response.json(definition, { status: 201 }); }
      const url = new URL(request.url); reads.push(url);
      return Response.json({ definitions: created && (definition.is_active || url.searchParams.get('include_hidden') === 'true') ? [definition] : [] });
    }
    if (path.endsWith('/attribute-definitions/41')) {
      if (request.method === 'DELETE') { writes.push(request); created = false; return new Response(null, { status: 204 }); }
      if (request.method === 'PATCH') {
        writes.push(request.clone()); const patch = await request.json();
        if (patch.is_active && conflict) { conflict = false; definition.revision = 3; definition.label = 'Concurrent color'; return Response.json({ message: 'Field changed elsewhere' }, { status: 409 }); }
        Object.assign(definition, patch, { revision: definition.revision + 1 });
      }
      return Response.json(definition, { headers: { ETag: `"attribute-definition-41-r${definition.revision}"` } });
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
  const fields = render(PersonFields, { controller });
  await fireEvent.click(screen.getByRole('button', { name: 'More fields' }));
  await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Favorite color' })).toBeDefined());
  expect(reads.at(-1)?.searchParams.has('include_hidden')).toBe(false);
  fields.unmount(); controller.destroy();
  let catalog = render(PeopleCatalogSettings, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
  await screen.findByRole('textbox', { name: 'Name' });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await screen.findByText('text · Inactive');
  expect(writes[1]!.headers.get('If-Match')).toBe('"attribute-definition-41-r1"');
  catalog.unmount(); catalog = render(PeopleCatalogSettings, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Edit Preferred color' }));
  await screen.findByRole('textbox', { name: 'Name' });
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await screen.findByText('Field changed elsewhere');
  await fireEvent.click(screen.getByRole('button', { name: 'Reload field' }));
  await waitFor(() => expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Concurrent color'));
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await screen.findByRole('button', { name: 'Edit Concurrent color' });
  expect(writes[3]!.headers.get('If-Match')).toBe('"attribute-definition-41-r3"');
  await expect(writes[3]!.clone().json()).resolves.toMatchObject({ is_active: true });
  await fireEvent.click(screen.getByRole('button', { name: 'Edit Concurrent color' }));
  await screen.findByRole('textbox', { name: 'Name' });
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete field' }));
  await screen.findByText('No custom fields yet');
  expect(writes.at(-1)!.headers.get('If-Match')).toBe('"attribute-definition-41-r4"');
  catalog.unmount();
});
