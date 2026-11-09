import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import PeopleCatalogSettings from './PeopleCatalogSettings.svelte';

it.each([
  { operation: 'conflict recovery', name: 'recovers a catalog edit from a revision conflict' },
  { operation: 'edit during retry', name: 'retains a catalog edit when an earlier registry retry finishes' },
  { operation: 'delete during retry', name: 'retains a catalog deletion when an earlier registry retry finishes' },
])('$name', async ({ operation }) => {
  const delayed = operation.endsWith('during retry');
  let created = true; let conflict = operation === 'conflict recovery';
  const writes: Request[] = [];
  const definition = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: operation !== 'conflict recovery', is_sensitive: false, revision: 1, is_deletable: true };
  let reads = 0;
  let releaseRetry!: (response: Response) => void;
  const pendingRetry = new Promise<Response>(resolve => { releaseRetry = resolve; });
  const staleResponse = Response.json({ definitions: [definition] });
  const reloads = vi.spyOn(DirectoryProfileController.prototype, 'reloadDefinitions');
  let retry: Promise<unknown> | undefined;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      reads += 1;
      if (delayed && reads === 2) return Response.json({ message: 'Registry unavailable' }, { status: 503 });
      if (delayed && reads === 3) return pendingRetry;
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
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    if (delayed) {
      await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
      await screen.findByRole('textbox', { name: 'Name' });
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
      await fireEvent.click(await screen.findByRole('button', { name: 'Retry fields' }));
      await waitFor(() => expect(reads).toBe(3));
      retry = reloads.mock.results.at(-1)!.value;
    }
    await fireEvent.click(await screen.findByRole('button', { name: 'Edit Favorite color' }));
    await screen.findByRole('textbox', { name: 'Name' });
    if (operation.startsWith('delete')) {
      await fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete field' }));
      await screen.findByText('No fields yet');
    } else {
      if (operation.startsWith('edit')) await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Preferred color' } });
      else await fireEvent.click(screen.getByRole('checkbox', { name: 'Active' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
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
        await expect(writes.at(-1)!.clone().json()).resolves.toMatchObject({ label: 'Preferred color' });
      }
    }
    expect(writes[0]!.headers.get('If-Match')).toBe('"attribute-definition-41-r1"');
    if (delayed) {
      expect(writes[1]!.headers.get('If-Match')).toBe('"attribute-definition-41-r2"');
      await act(async () => { releaseRetry(staleResponse); await retry; });
      expect(screen.queryByRole('button', { name: 'Edit Favorite color' })).toBeNull();
      if (operation.startsWith('delete')) expect(screen.getByText('No fields yet')).toBeDefined();
      else expect(screen.getByRole('button', { name: 'Edit Preferred color' })).toBeDefined();
    }
  } finally { releaseRetry(staleResponse); view.unmount(); reloads.mockRestore(); }
});

it.each(['loading and retry gates', 'field order and labels with search by name and description'])('shows catalog %s', async scenario => {
  const recovery = scenario === 'loading and retry gates';
  let targetsRead = 0;
  let releaseTargets!: (response: Response) => void;
  const pendingTargets = recovery ? new Promise<Response>(resolve => { releaseTargets = resolve; }) : undefined;
  let initialRead = true;
  let releaseInitial!: (response: Response) => void;
  const pendingInitial = recovery ? new Promise<Response>(resolve => { releaseInitial = resolve; }) : undefined;
  const definitions: Record<string, unknown>[] = [
    { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'favorite_color', label: 'Favorite color', description: 'Garden preferences', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 2, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: true, revision: 1, is_deletable: true },
    { id: 42, universal_id: '00000000-0000-4000-8000-000000000042', object_type: 'person', slug: 'location', label: 'Location', description: 'Where they live', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 1, ownership: 'system', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true }
  ];
  const targets = [{ kind: 'employment', key: 'system:employment', description: 'Current and historical jobs', cardinality: 'multi', sensitive: false }];
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input); const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (recovery && initialRead) { initialRead = false; return pendingInitial!; }
      return Response.json({ definitions });
    }
    if (path.endsWith('/person-fact-targets')) {
      if (!recovery) return Response.json({ targets });
      return ++targetsRead === 1 ? Response.json({ message: 'Targets unavailable' }, { status: 503 }) : pendingTargets!;
    }
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    const fields = within(screen.getByRole('region', { name: 'Fields' }));
    if (recovery) {
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
      releaseTargets(Response.json({ targets }));
      expect(await fields.findByText('Current and historical jobs')).toBeDefined();
    } else {
      await fields.findByText('Current and historical jobs');
      await fields.findByText('Garden preferences');
      expect([...view.container.querySelectorAll('.fields .name')].map(row => row.firstChild?.textContent)).toEqual(['Work', 'Location', 'Favorite color']);
      expect(fields.getByText('Current and historical jobs').closest('li')?.textContent).toContain('Jobs · several values');
      expect(fields.getByText('Garden preferences').closest('li')?.textContent).toContain('Profile field · Text · Sensitive');
      expect(fields.queryByRole('button', { name: 'Edit Location' })).toBeNull();
      const search = screen.getByRole('searchbox', { name: 'Search fields' });
      await fireEvent.input(search, { target: { value: 'garden' } });
      expect(fields.getByText('Garden preferences')).toBeDefined();
      expect(fields.queryByText('Where they live')).toBeNull();
      expect(fields.queryByText('Current and historical jobs')).toBeNull();
      await fireEvent.input(search, { target: { value: 'location' } });
      expect(fields.getByText('Where they live')).toBeDefined();
      await fireEvent.input(search, { target: { value: 'plant' } });
      expect(screen.getByText('No fields match')).toBeDefined();
    }
  } finally { view.unmount(); }
});

it('keeps a committed field creation resumable after closing its catalog dialog', async () => {
  const created = { id: 41, universal_id: '00000000-0000-4000-8000-000000000041', object_type: 'person', slug: 'preferred_channel', label: 'Preferred channel', value_type: 'text', field_type: 'text', cardinality: 'single', display_order: 0, ownership: 'user', ui_creatable: true, ui_editable: true, api_mutable: true, is_active: true, is_sensitive: false, revision: 1, is_deletable: true };
  let committed = false;
  const client = createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path.endsWith('/attribute-definitions')) {
      if (request.method === 'POST') { committed = true; return Response.json(created, { status: 201 }); }
      return committed ? Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 }) : Response.json({ definitions: [] });
    }
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
    if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const view = render(PeopleCatalogSettings, { client });
  try {
    await fireEvent.click(await screen.findByRole('button', { name: 'Add field' }));
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('button', { name: 'Retry registry refresh' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
    expect(screen.queryByRole('dialog', { name: 'Create attribute field' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Resume field creation' }));
    expect(await screen.findByRole('dialog', { name: 'Create attribute field' })).toBeDefined();
  } finally { view.unmount(); }
});
