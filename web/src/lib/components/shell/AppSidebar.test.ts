import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { SavedView } from '../../api/generated/models';
import { parseExploreURLState } from '../../explore/state.svelte';
import type { JobsStore } from '../../jobs/jobs.svelte';
import AppSidebar from './AppSidebar.svelte';

const jobs = { items: [], health: 'ok', loaded: true, refresh: vi.fn() } as unknown as JobsStore;

function view(id: number, name: string, presentation = 'table'): SavedView {
  return {
    id,
    name,
    revision: 1,
    schema_version: 2,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    canonical_state: { filters: [], grouping: [], presentation, sort: [], columns: [] },
  };
}

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
    expect(footer.getByRole('button', { name: /Sync status: Synced/ })).toBeTruthy();
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

  it('opens a saved view from its row', async () => {
    const onOpenView = vi.fn();
    const saved = view(1, 'Budget threads');
    renderSidebar({ views: [saved], onOpenView });
    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads' }));
    expect(onOpenView).toHaveBeenCalledWith(saved);
  });

  it('does not open a view the app cannot read', async () => {
    const onOpenView = vi.fn();
    renderSidebar({ views: [{ ...view(1, 'Old view'), schema_version: 9 }], onOpenView });
    await fireEvent.click(screen.getByRole('button', { name: 'Old view' }));
    expect(onOpenView).not.toHaveBeenCalled();
  });

  it('edits a view description from its menu', async () => {
    const onDescribeView = vi.fn(async () => undefined);
    const saved = view(1, 'Budget threads');
    renderSidebar({ views: [saved], onDescribeView });

    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Edit description…' }));
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Description' }), { target: { value: 'Q3 numbers' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onDescribeView).toHaveBeenCalledWith(saved, 'Q3 numbers'));
  });

  it('renames a view in place on Enter and cancels on Escape', async () => {
    const onRenameView = vi.fn(async () => undefined);
    const saved = view(1, 'Budget threads');
    renderSidebar({ views: [saved], onRenameView });

    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const input = (await screen.findByRole('textbox', { name: 'Rename Budget threads' })) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Q3 budget' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(onRenameView).toHaveBeenCalledWith(saved, 'Q3 budget'));
    await waitFor(() => expect(screen.queryByRole('textbox', { name: /Rename/ })).toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const again = await screen.findByRole('textbox', { name: 'Rename Budget threads' });
    await fireEvent.keyDown(again, { key: 'Escape' });
    expect(screen.queryByRole('textbox', { name: /Rename/ })).toBeNull();
    expect(onRenameView).toHaveBeenCalledOnce();
  });

  it('keeps the rename field open and shows the reason when the rename fails', async () => {
    const onRenameView = vi.fn(async () => 'This view changed in another session.');
    renderSidebar({ views: [view(1, 'Budget threads')], onRenameView });
    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const input = await screen.findByRole('textbox', { name: 'Rename Budget threads' });
    await fireEvent.input(input, { target: { value: 'Q3' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect((await screen.findByRole('alert')).textContent).toContain('changed in another session');
    expect(screen.getByRole('textbox', { name: 'Rename Budget threads' })).toBeTruthy();
  });

  it('deletes a view from its menu', async () => {
    const onDeleteView = vi.fn();
    const saved = view(1, 'Budget threads');
    renderSidebar({ views: [saved], onDeleteView });
    await fireEvent.click(screen.getByRole('button', { name: 'Budget threads actions' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    expect(onDeleteView).toHaveBeenCalledWith(saved);
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
        { id: 'f', name: 'Work Gmail', state: 'failed', detail: 'Gmail sign-in expired', target: { kind: 'source', id: 4 } },
        { id: 'r', name: 'Contacts', state: 'running', detail: 'Syncing', target: { kind: 'none' } },
      ],
      health: 'failed',
      loaded: true,
      refresh: vi.fn(),
    } as unknown as JobsStore;
    renderSidebar({ jobs: store, onOpenJob });
    await fireEvent.click(screen.getByRole('button', { name: /Sync status: Needs attention/ }));
    expect(store.refresh).toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('menuitem', { name: /Work Gmail/ }));
    expect(onOpenJob).toHaveBeenCalledWith({ kind: 'source', id: 4 });
  });

  it('opens Help and toggles the rail from the footer', async () => {
    const props = renderSidebar();
    await fireEvent.click(screen.getByRole('button', { name: 'Collapse sidebar' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Help' }));
    expect(props.onToggleCollapsed).toHaveBeenCalled();
    expect(props.onOpenHelp).toHaveBeenCalled();
  });

  it('keeps focus on the toggle when the parent switches the sidebar to the rail', async () => {
    const { rerender } = renderSidebar();
    const toggle = screen.getByRole('button', { name: 'Collapse sidebar' });
    toggle.focus();
    await fireEvent.click(toggle);
    await rerender({ collapsed: true });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Expand sidebar' }));
  });
});
