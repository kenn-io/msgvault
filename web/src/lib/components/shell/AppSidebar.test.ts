import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { view } from './fixtures.test-support';
import { parseExploreURLState } from '../../explore/state.svelte';
import type { JobsStore } from '../../jobs/jobs.svelte';
import AppSidebar from './AppSidebar.svelte';

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
    renderSidebar();
    const footer = within(screen.getByRole('complementary', { name: 'Sidebar' }));
    expect(footer.getByRole('button', { name: 'Settings' })).toBeTruthy();
    expect(footer.getByRole('button', { name: /Sync status: Idle/ })).toBeTruthy();
    expect(footer.queryByRole('button', { name: /Keyboard shortcuts/ })).toBeNull();
    expect(footer.getByRole('button', { name: 'Help' })).toBeTruthy();
  });

  it('nests saved views under the place they open', () => {
    renderSidebar({ views: [view(1, 'Budget threads'), { ...view(2, 'PDF invoices', 'files'), schema_version: 1 }] });
    const messages = within(screen.getByRole('list', { name: 'Messages saved views' }));
    const files = within(screen.getByRole('list', { name: 'Files saved views' }));
    expect(messages.getByRole('button', { name: 'Budget threads' })).toBeTruthy();
    expect(messages.queryByRole('button', { name: 'PDF invoices' })).toBeNull();
    expect(files.getByRole('button', { name: 'PDF invoices' })).toBeTruthy();
  });

  it('shows what a saved view holds in its tooltip', () => {
    const saved = view(1, 'Budget threads');
    saved.description = 'Every thread about the Q3 budget';
    saved.canonical_state = { ...(saved.canonical_state as object), query: 'budget' };
    renderSidebar({ views: [saved, view(2, 'Plain')] });
    expect(screen.getByRole('button', { name: 'Budget threads' }).getAttribute('title')).toBe(
      'Full text: “budget” · Table\nEvery thread about the Q3 budget',
    );
    expect(screen.getByRole('button', { name: 'Plain' }).getAttribute('title')).toBe('Table');
  });

  it.each([
    { schema_version: 3, canonical_state: { presentation: 'files', filters: { source: ['1'] } }, group: 'Messages' },
    { schema_version: 2, incompatibility_reason: 'Stored filters are incompatible.', canonical_state: { filters: { source: ['1'] } }, group: 'Messages' },
    { schema_version: 2, incompatibility_reason: 'invalid saved view canonical state: semantic and hybrid exploration require free text', canonical_state: { presentation: 'files', search_mode: 'semantic', filters: [] }, group: 'Files' },
    { schema_version: 2, incompatibility_reason: 'Stored filters are incompatible.', canonical_state: { presentation: 'files', filters: { source: ['1'] } }, group: 'Files' },
  ])('keeps an incompatible schema $schema_version view disabled and deletable under $group', async ({ group, ...incompatible }) => {
    const onOpenView = vi.fn();
    const onDeleteView = vi.fn();
    renderSidebar({ views: [{ ...view(1, 'Old view'), ...incompatible }], onOpenView, onDeleteView });
    const saved = within(screen.getByRole('list', { name: `${group} saved views` })).getByRole('button', { name: 'Old view' });
    expect(saved.getAttribute('aria-disabled')).toBe('true');
    await fireEvent.click(saved);
    expect(onOpenView).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', { name: 'Old view actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Rename' })).toBeNull();
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    expect(onDeleteView).toHaveBeenCalledOnce();
  });

  it.each(['save', 'move'])('locks a pending rename and preserves focus through %s and cancel', async ending => {
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
    expect(input.readOnly).toBe(true);
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(onRenameView).toHaveBeenCalledOnce();
    const files = primary().getByRole('button', { name: 'Files' });
    if (ending === 'move') files.focus();
    finish();
    await waitFor(() => expect(screen.queryByRole('textbox', { name: /Rename/ })).toBeNull());
    await tick();
    expect(document.activeElement).toBe(ending === 'move' ? files : screen.getByRole('button', { name: 'Budget threads' }));

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
    expect(screen.getByRole('menuitem', { name: /Work Gmail/ }).textContent).toContain('Gmail sign-in expired');
    expect(screen.getByRole('menuitem', { name: /CardDAV sync/ }).textContent).toContain('Running');
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
  renderSidebar({ views: [view(1, 'Budget')], viewsError: "Couldn't load saved views.", onRetryViews });
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
  const saved = { ...view(1, 'Budget threads'), description: 'Original description' };
  const onRenameView = vi.fn(async () => 'Review the latest view.');
  const onDescribeView = vi.fn(async () => 'Review the latest view.');
  const { rerender } = renderSidebar({ views: [saved], onRenameView, onDescribeView });
  await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: field === 'name' ? 'Rename' : 'Edit description…' }));
  const editor = await screen.findByRole('textbox', { name: field === 'name' ? 'Rename Budget threads' : 'Description' });
  await fireEvent.input(editor, { target: { value: 'My stale edit' } });
  await rerender({ views: [{ ...saved, [field]: 'Other browser edit', revision: 2 }] });
  if (field === 'name') await fireEvent.keyDown(editor, { key: 'Enter' });
  else await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Review the latest view.');
  const handler = field === 'name' ? onRenameView : onDescribeView;
  expect(handler).toHaveBeenCalledWith(saved, 'My stale edit');
});
