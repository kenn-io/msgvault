import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PeopleCatalogSettings from './PeopleCatalogSettings.svelte';

it('retains catalog edit, deactivate, conflict recovery, reactivate, and delete', async () => {
  let created = true; let conflict = true;
  const writes: Request[] = [];
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true };
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      const url = new URL(request.url);
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
  const catalog = render(PeopleCatalogSettings, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
  await screen.findByRole('textbox', { name: 'Name' });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await screen.findByText('Text · inactive');
  expect(writes[0]!.headers.get('If-Match')).toBe('"attribute-definition-41-r1"');
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
  expect(writes[2]!.headers.get('If-Match')).toBe('"attribute-definition-41-r3"');
  await expect(writes[2]!.clone().json()).resolves.toMatchObject({ is_active: true });
  await fireEvent.click(screen.getByRole('button', { name: 'Edit Concurrent color' }));
  await screen.findByRole('textbox', { name: 'Name' });
  await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete field' }));
  await screen.findByText('No custom fields yet');
  expect(writes.at(-1)!.headers.get('If-Match')).toBe('"attribute-definition-41-r4"');
  catalog.unmount();
});

it('keeps Add disabled until a resumed field creation is acknowledged', async () => {
  let failRefresh = false; const definitions: Record<string, unknown>[] = [];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (request.method === 'POST') {
        const definition = { ...(await request.json()), id: 41, universal_id: '00000000-0000-4000-8000-000000000041', slug: 'first_field', ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, revision: 1 };
        definitions.push(definition); failRefresh = true;
        return Response.json(definition, { status: 201 });
      }
      if (failRefresh) { failRefresh = false; return Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 }); }
      return Response.json({ definitions });
    }
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const registry = vi.spyOn(DirectoryProfileController.prototype, 'reloadDefinitions');
  const view = render(PeopleCatalogSettings, { client });
  try {
    await act(async () => { await registry.mock.results[0]!.value; });
    const fields = registry.mock.contexts[0] as DirectoryProfileController;
    await act(async () => { await fields.createDefinition({ object_type: 'person', label: 'First field', value_type: 'text', field_type: 'text', cardinality: 'single', is_sensitive: false, display_order: 0 }); });
    expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Resume field creation' }));
    await act(async () => { await fields.retryDefinitionRefresh(); });
    const done = await screen.findByRole('button', { name: 'Done' });
    expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', true);
    await fireEvent.click(done);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', false));
  } finally { registry.mockRestore(); view.unmount(); }
});
