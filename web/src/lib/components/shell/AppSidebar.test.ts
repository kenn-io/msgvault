import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { view } from './fixtures.test-support';
import { parseExploreURLState } from '../../explore/state.svelte';
import type { JobsStore } from '../../jobs/jobs.svelte';
import AppSidebar from './AppSidebar.svelte';
import { ShellData } from './shell-data.svelte';
import { createAPIClient } from '../../api/client';

const jobs = { items: [], health: 'ok', loaded: true, refresh: vi.fn() } as unknown as JobsStore;

function renderSidebar(overrides: Record<string, unknown> = {}) {
  const props = {
    active: 'everything' as const,
    collapsed: false,
    showCollapseToggle: true,
    jobs,
    onPastRuns: vi.fn(),
    onOpenJob: vi.fn(),
    onNavigate: vi.fn(),
    onToggleCollapsed: vi.fn(),
    onOpenHelp: vi.fn(),
    ...overrides,
  };
  const view = render(AppSidebar, props);
  return { ...props, rerender: view.rerender, container: view.container };
}

const primary = () => within(screen.getByRole('navigation', { name: 'Primary' }));

describe('AppSidebar', () => {
  it('lists four places with the active one current', () => {
    renderSidebar();
    expect(
      primary()
        .getAllByRole('button')
        .map((b) => b.getAttribute('aria-label')),
    ).toEqual(['Messages', 'People', 'Files', 'Sources']);
    expect(primary().getByRole('button', { name: 'Messages' }).getAttribute('aria-current')).toBe('page');
    expect(primary().getByRole('button', { name: 'Files' }).hasAttribute('aria-current')).toBe(false);
  });

  it.each(['directory', 'directory_review'])('keeps People current when opening an old %s link', (workspace) => {
    renderSidebar({ active: parseExploreURLState(`?workspace=${workspace}`).workspace });
    expect(primary().getByRole('button', { name: 'People' }).getAttribute('aria-current')).toBe('page');
  });

  it('puts Settings, Help, and the status in the footer, with no Keyboard shortcuts row', () => {
    const props = renderSidebar();
    const footer = within(screen.getByRole('complementary', { name: 'Sidebar' }));
    expect(footer.getByRole('button', { name: 'Settings' })).toBeTruthy();
    expect(footer.getByRole('button', { name: /Sync status: Idle/ })).toBeTruthy();
    expect(footer.queryByRole('button', { name: /Keyboard shortcuts/ })).toBeNull();
    expect(props.onOpenHelp).not.toHaveBeenCalled();
  });

  it('nests saved views under the place they open', () => {
    renderSidebar({ views: [view(1, 'Budget threads'), view(2, 'PDF invoices', 'files')] });
    const messages = within(screen.getByRole('list', { name: 'Messages saved views' }));
    const files = within(screen.getByRole('list', { name: 'Files saved views' }));
    expect(messages.getByRole('button', { name: 'Budget threads' })).toBeTruthy();
    expect(messages.queryByRole('button', { name: 'PDF invoices' })).toBeNull();
    expect(files.getByRole('button', { name: 'PDF invoices' })).toBeTruthy();
  });

  it('shows what a saved view holds in its tooltip', () => {
    const saved = {
      ...view(1, 'Budget threads'),
      description: 'Every thread about the Q3 budget',
      canonical_state: { filters: [], grouping: [], presentation: 'table', sort: [], columns: [], query: 'budget' },
    };
    renderSidebar({ views: [saved, view(2, 'Plain')] });
    expect(screen.getByRole('button', { name: 'Budget threads' }).getAttribute('title')).toBe(
      'Full text: “budget” · Table\nEvery thread about the Q3 budget',
    );
    expect(screen.getByRole('button', { name: 'Plain' }).getAttribute('title')).toBe('Table');
  });

  it('does not open a view the app cannot read', async () => {
    const onOpenView = vi.fn();
    renderSidebar({ views: [{ ...view(1, 'Old view'), schema_version: 9 }], onOpenView });
    await fireEvent.click(screen.getByRole('button', { name: 'Old view' }));
    expect(onOpenView).not.toHaveBeenCalled();
  });

  it('locks a pending rename and returns focus after saving or canceling', async () => {
    let finish!: () => void;
    const pending = new Promise<undefined>(resolve => { finish = () => resolve(undefined); });
    const onRenameView = vi.fn(() => pending);
    const saved = view(1, 'Budget threads');
    renderSidebar({ views: [saved], onRenameView });

    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const input = (await screen.findByRole('textbox', { name: 'Rename Budget threads' })) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Q3 budget' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(onRenameView).toHaveBeenCalledWith(saved, 'Q3 budget'));
    expect(input.disabled).toBe(true);
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(onRenameView).toHaveBeenCalledOnce();
    finish();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Budget threads' })));

    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const again = await screen.findByRole('textbox', { name: 'Rename Budget threads' });
    await fireEvent.keyDown(again, { key: 'Escape' });
    expect(screen.queryByRole('textbox', { name: /Rename/ })).toBeNull();
    expect(onRenameView).toHaveBeenCalledOnce();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Budget threads' })));
  });

  it('keeps full accessible names in the icon rail and hides saved views', () => {
    renderSidebar({ collapsed: true, views: [view(1, 'Budget threads')] });
    expect(screen.getByRole('button', { name: 'Messages' }).getAttribute('aria-current')).toBe('page');
    expect(screen.getByRole('button', { name: 'Settings' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Budget threads' })).toBeNull();
    expect(screen.queryByText('Settings')).toBeNull();
  });

  it('lists running jobs and failures in the status menu and opens their source', async () => {
    const onOpenJob = vi.fn();
    const store = {
      items: [
        { id: 'f', name: 'Work Gmail', state: 'failed', detail: 'Gmail sign-in expired', target: { kind: 'source' } },
        { id: 'r', name: 'CardDAV sync', state: 'running', detail: 'Running', target: { kind: 'status', authority: 'getCardDAVStatus' } },
      ],
      health: 'failed',
      loaded: true,
      refresh: vi.fn(),
    } as unknown as JobsStore;
    renderSidebar({ jobs: store, onOpenJob });
    await fireEvent.click(screen.getByRole('button', { name: /Sync status: Needs attention/ }));
    expect(store.refresh).toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('menuitem', { name: /Work Gmail/ }));
    expect(onOpenJob).toHaveBeenCalledWith({ kind: 'source' });
  });

  it('keeps focus on the toggle when the parent switches the sidebar to the rail', async () => {
    const { rerender, onToggleCollapsed } = renderSidebar();
    const toggle = screen.getByRole('button', { name: 'Collapse sidebar' });
    toggle.focus();
    await fireEvent.click(toggle);
    expect(onToggleCollapsed).toHaveBeenCalledOnce();
    await rerender({ collapsed: true });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Expand sidebar' }));
  });
});

it('shows a saved-view read error above the last rows and offers Retry', async () => {
  const onRetryViews = vi.fn();
  renderSidebar({ views: [view(1, 'Budget')], viewsError: 'Unavailable', onRetryViews });
  expect(screen.getByRole('alert').textContent).toContain("Couldn't load saved views.");
  expect(screen.getByRole('button', { name: 'Budget' })).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(onRetryViews).toHaveBeenCalledOnce();
});

it('shows neutral Loading before the first job status fetch', () => {
  renderSidebar({ jobs: { ...jobs, loaded: false, health: 'unavailable' } });
  expect(screen.getByRole('button', { name: 'Sync status: Loading' })).toBeTruthy();
});

it.each(['name', 'description'] as const)('keeps the original %s revision when a refresh updates an open editor', async field => {
  let saved = { ...view(1, 'Budget threads'), description: 'Original description' };
  const revisions: string[] = [];
  const data = new ShellData(createAPIClient(async input => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (request.method === 'PATCH') {
      const revision = request.headers.get('If-Match')!;
      revisions.push(revision);
      if (revision !== `"saved-view-1-r${saved.revision}"`) return Response.json({}, { status: 409 });
      saved = { ...saved, ...await request.json(), revision: saved.revision + 1 };
      return Response.json(saved);
    }
    if (path.endsWith('/saved-views')) return Response.json({ saved_views: [saved] });
    return Response.json(path.endsWith('/sources/status') ? { sources: [] } : { lanes: [] });
  }));
  await data.refresh();
  const { rerender } = renderSidebar({
    views: data.views,
    onRenameView: (saved: typeof data.views[number], name: string) => data.renameView(saved, name),
    onDescribeView: (saved: typeof data.views[number], description: string) => data.describeView(saved, description),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: field === 'name' ? 'Rename' : 'Edit description…' }));
  const editor = await screen.findByRole('textbox', { name: field === 'name' ? 'Rename Budget threads' : 'Description' });
  await fireEvent.input(editor, { target: { value: 'My stale edit' } });
  saved = { ...saved, [field]: 'Other browser edit', revision: 2 };
  await data.refresh();
  await rerender({ views: data.views });
  if (field === 'name') await fireEvent.keyDown(editor, { key: 'Enter' });
  else await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Cancel and reopen the editor to review the latest view.');
  expect(revisions).toEqual(['"saved-view-1-r1"']);
  expect(saved[field]).toBe('Other browser edit');
  await waitFor(() => expect(data.views[0]?.revision).toBe(2));
  data.stop();
});
