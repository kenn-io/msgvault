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
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
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
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull());
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
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
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
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull());
    } else {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
      await screen.findByRole('button', { name: 'Edit Preferred color' });
    }

    await act(async () => { releaseRetry(staleResponse); await retry; });
    expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull();
    if (operation === 'delete') expect(screen.queryByText('No fields match')).toBeNull();
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
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
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

it('lists Work and system and custom fields, searches names and descriptions, and edits custom fields only', async () => {
  const base = { object_type: 'person', value_type: 'text', field_type: 'text', cardinality: 'single', is_active: true, revision: 1 };
  const definitions = [
    { ...base, id: 2, universal_id: 'favorite-plant', slug: 'favorite_plant', label: 'Favorite plant', description: 'Garden preferences', ownership: 'user', display_order: 2, is_sensitive: true },
    { ...base, id: 1, universal_id: 'location', slug: 'location', label: 'Location', description: 'Where they live', ownership: 'system', display_order: 1 },
  ];
  let targetsRead = 0;
  let releaseTargets!: (response: Response) => void;
  const pendingTargets = new Promise<Response>(resolve => { releaseTargets = resolve; });
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions });
    if (path.endsWith('/person-fact-targets') && ++targetsRead === 1) return Response.json({ message: 'Targets unavailable' }, { status: 503 });
    if (path.endsWith('/person-fact-targets')) return pendingTargets;
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0 });
    throw new Error(`Unexpected ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    const fields = within(screen.getByRole('region', { name: 'Fields' }));
    await fields.findByText('Garden preferences');
    expect(fields.getByText("Couldn't load Work details.")).toBeDefined();
    const search = fields.getByRole('searchbox', { name: 'Search fields' });
    await fireEvent.input(search, { target: { value: 'Work' } });
    await fireEvent.click(await fields.findByRole('button', { name: 'Retry' }));
    expect(fields.getByText('Loading fields…')).toBeDefined();
    expect(fields.queryByText('No fields match')).toBeNull();
    releaseTargets(Response.json({ targets: [{ kind: 'employment', key: 'system:employment', description: 'Current and historical jobs', cardinality: 'multi', sensitive: false }] }));
    const work = await fields.findByText('Current and historical jobs');
    expect(work.closest('li')?.textContent).toContain('Jobs · several values');
    await fireEvent.input(search, { target: { value: '' } });
    await fields.findByText('Garden preferences');
    expect([...view.container.querySelectorAll('.fields .name')].map(row => row.firstChild?.textContent)).toEqual(['Work', 'Location', 'Favorite plant']);
    expect(fields.getByText('Garden preferences').closest('li')?.textContent).toContain('Profile field · Text · Sensitive');
    expect(fields.queryByRole('button', { name: 'Edit Location' })).toBeNull();
    expect(fields.getByRole('button', { name: 'Edit Favorite plant' })).toBeDefined();
    await fireEvent.input(search, { target: { value: 'garden' } });
    expect(fields.queryByText('Where they live')).toBeNull();
    expect(fields.queryByText('Current and historical jobs')).toBeNull();
    expect(fields.getByText('Garden preferences')).toBeDefined();
    await fireEvent.input(search, { target: { value: 'location' } });
    expect(fields.getByText('Where they live')).toBeDefined();
    await fireEvent.input(search, { target: { value: 'no match' } });
    expect(fields.getByText('No fields match')).toBeDefined();
  } finally { view.unmount(); }
});

it('shows loading and failure before an empty catalog', async () => {
  let reads = 0;
  let release!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { release = resolve; });
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) return ++reads === 1 ? pending : Response.json({ definitions: [] });
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    expect(screen.getByText('Loading fields…')).toBeDefined();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search fields' }), { target: { value: 'plant' } });
    expect(screen.queryByText('No fields match')).toBeNull();
    release(Response.json({ message: 'Fields unavailable' }, { status: 503 }));
    await screen.findByRole('button', { name: 'Retry fields' });
    expect(screen.queryByText('No fields match')).toBeNull();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search fields' }), { target: { value: 'plant' } });
    expect(screen.queryByText('No fields match')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry fields' }));
    await screen.findByText('No fields match');
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search fields' }), { target: { value: '' } });
    expect(screen.getByText('No fields yet')).toBeDefined();
  } finally { release(Response.json({ definitions: [] })); view.unmount(); }
});
