import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PeopleCatalogSettings from './PeopleCatalogSettings.svelte';

it.each(['edit', 'conflict recovery', 'delete'])('retains catalog %s', async operation => {
  let created = true; let conflict = operation === 'conflict recovery';
  const writes: Request[] = [];
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: operation !== 'conflict recovery', is_sensitive: false, revision: 1, is_deletable: true };
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
        if (patch.is_active && conflict) { conflict = false; definition.revision = 3; definition.label = 'Concurrent color'; return Response.json({ error: 'attribute_definition_revision_conflict', message: 'Field changed elsewhere' }, { status: 409 }); }
        Object.assign(definition, patch, { revision: definition.revision + 1 });
      }
      return Response.json(definition, { headers: { ETag: `"attribute-definition-41-r${definition.revision}"` } });
    }
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${path}`);
  });
  const catalog = render(PeopleCatalogSettings, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
    await screen.findByRole('textbox', { name: 'Name' });
    if (operation === 'delete') {
      await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete field' }));
      await screen.findByText('No custom fields yet');
      expect(writes[0]!.headers.get('If-Match')).toBe('"attribute-definition-41-r1"');
      return;
    }
    if (operation === 'edit') await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
    else await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    expect(writes[0]!.headers.get('If-Match')).toBe('"attribute-definition-41-r1"');
    if (operation === 'conflict recovery') {
      await expect(writes[0]!.clone().json()).resolves.toMatchObject({ is_active: true });
      await screen.findByText('Field changed elsewhere');
      await fireEvent.click(screen.getByRole('button', { name: 'Reload field' }));
      await waitFor(() => expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Concurrent color'));
      await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
      await screen.findByRole('button', { name: 'Edit Concurrent color' });
      expect(writes[1]!.headers.get('If-Match')).toBe('"attribute-definition-41-r3"');
      await expect(writes[1]!.clone().json()).resolves.toMatchObject({ is_active: true });
    } else {
      await screen.findByRole('button', { name: 'Edit Preferred color' });
      await expect(writes[0]!.clone().json()).resolves.toMatchObject({ label: 'Preferred color' });
    }
  } finally { catalog.unmount(); }
});

it.each(['edit', 'delete'])('ignores a delayed registry retry after a catalog %s', async operation => {
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true };
  let definitions = [definition];
  let reads = 0;
  let releaseRetry!: (response: Response) => void;
  const pendingRetry = new Promise<Response>(resolve => { releaseRetry = resolve; });
  const staleResponse = Response.json({ definitions });
  const reloads = vi.spyOn(DirectoryProfileController.prototype, 'reloadDefinitions');
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      reads += 1;
      if (reads === 2) return Response.json({ message: 'Registry unavailable' }, { status: 503 });
      if (reads === 3) return pendingRetry;
      return Response.json({ definitions });
    }
    if (path.endsWith('/attribute-definitions/41')) {
      if (request.method === 'DELETE') { definitions = []; return new Response(null, { status: 204 }); }
      if (request.method === 'PATCH') definitions = [{ ...definition, ...await request.json(), revision: definition.revision + 1 }];
      return Response.json(definitions[0], { headers: { ETag: `"attribute-definition-41-r${definitions[0]!.revision}"` } });
    }
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
    await screen.findByRole('textbox', { name: 'Name' });
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Retry fields' }));
    await waitFor(() => expect(reads).toBe(3));
    const retry = reloads.mock.results.at(-1)!.value;

    await fireEvent.click(screen.getByRole('button', { name: 'Edit Favorite color' }));
    await screen.findByRole('textbox', { name: 'Name' });
    if (operation === 'delete') {
      await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete field' }));
      await screen.findByText('No custom fields yet');
    } else {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
      await screen.findByRole('button', { name: 'Edit Preferred color' });
    }

    await act(async () => { releaseRetry(staleResponse); await retry; });
    expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull();
    if (operation === 'delete') expect(screen.getByText('No custom fields yet')).toBeDefined();
    else expect(screen.getByRole('button', { name: 'Edit Preferred color' })).toBeDefined();
  } finally {
    releaseRetry(staleResponse);
    view.unmount();
    reloads.mockRestore();
  }
});

it('keeps Add disabled and resumes a field creation after its refresh fails', async () => {
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
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Add field' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Label' }), { target: { value: 'First field' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await within(screen.getByRole('region', { name: 'Fields' })).findByRole('alert');
    await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
    expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Resume field creation' }));
    expect(await screen.findByRole('dialog', { name: 'Create attribute field' })).toBeDefined();
  } finally { view.unmount(); }
});
