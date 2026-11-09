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
      await screen.findByText('No fields yet');
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

it.each(['edit', 'delete'])('keeps catalog reads current during %s', async operation => {
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
      await screen.findByText('No fields yet');
    } else {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
      await screen.findByRole('button', { name: 'Edit Preferred color' });
    }

    await act(async () => { releaseRetry(staleResponse); await retry; });
    expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull();
    if (operation === 'delete') expect(screen.getByText('No fields yet')).toBeDefined();
    else expect(screen.getByRole('button', { name: 'Edit Preferred color' })).toBeDefined();
  } finally {
    releaseRetry(staleResponse);
    view.unmount();
    reloads.mockRestore();
  }
});

it('searches fields through definition loading and Work retry', async () => {
  let targetsRead = 0;
  let releaseTargets!: (response: Response) => void;
  const pendingTargets = new Promise<Response>(resolve => { releaseTargets = resolve; });
  let initialRead = true;
  let releaseInitial!: (response: Response) => void;
  const pendingInitial = new Promise<Response>(resolve => { releaseInitial = resolve; });
  const definitions: Record<string, unknown>[] = [
    { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', description: 'Garden preferences', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 2, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: true, revision: 1, is_deletable: true },
    { id: 42, universal_id: '00000000-0000-4000-8000-000000000042', object_type: 'person', slug: 'location', label: 'Location', description: 'Where they live', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 1, ownership: 'system', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true }
  ];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (initialRead) { initialRead = false; return pendingInitial; }
      return Response.json({ definitions });
    }
    if (path.endsWith('/person-fact-targets')) return ++targetsRead === 1 ? Response.json({ message: 'Targets unavailable' }, { status: 503 }) : pendingTargets;
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    const fields = within(screen.getByRole('region', { name: 'Fields' }));
    expect(screen.getByText('Loading fields…')).toBeDefined();
    const search = screen.getByRole('searchbox', { name: 'Search fields' });
    await fireEvent.input(search, { target: { value: 'plant' } });
    expect(screen.queryByText('No fields match')).toBeNull();
    releaseInitial(Response.json({ message: 'Fields unavailable' }, { status: 503 }));
    await screen.findByRole('button', { name: 'Retry fields' });
    expect(screen.queryByText('No fields match')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry fields' }));
    await fireEvent.input(search, { target: { value: '' } });
    await fields.findByText('Garden preferences');
    expect(fields.getByText("Couldn't load Work details.")).toBeDefined();
    await fireEvent.input(search, { target: { value: 'Work' } });
    await fireEvent.click(fields.getByRole('button', { name: 'Retry' }));
    expect(fields.getByText('Loading fields…')).toBeDefined();
    expect(fields.queryByText('No fields match')).toBeNull();
    releaseTargets(Response.json({ targets: [{ kind: 'employment', key: 'system:employment', description: 'Current and historical jobs', cardinality: 'multi', sensitive: false }] }));
    const work = await fields.findByText('Current and historical jobs');
    expect(work.closest('li')?.textContent).toContain('Jobs · several values');
    await fireEvent.input(search, { target: { value: '' } });
    expect([...view.container.querySelectorAll('.fields .name')].map(row => row.firstChild?.textContent)).toEqual(['Work', 'Location', 'Favorite color']);
    expect(fields.getByText('Garden preferences').closest('li')?.textContent).toContain('Profile field · Text · Sensitive');
    expect(fields.queryByRole('button', { name: 'Edit Location' })).toBeNull();
    await fireEvent.input(search, { target: { value: 'garden' } });
    expect(fields.getByText('Garden preferences')).toBeDefined();
    expect(fields.queryByText('Where they live')).toBeNull();
    expect(fields.queryByText('Current and historical jobs')).toBeNull();
    await fireEvent.input(search, { target: { value: 'location' } });
    expect(fields.getByText('Where they live')).toBeDefined();
    await fireEvent.input(search, { target: { value: '' } });
    await fireEvent.input(search, { target: { value: 'plant' } });
    expect(screen.getByText('No fields match')).toBeDefined();
    await fireEvent.input(search, { target: { value: '' } });
  } finally { releaseInitial(Response.json({ definitions: [] })); releaseTargets(Response.json({ targets: [] })); view.unmount(); }
});
