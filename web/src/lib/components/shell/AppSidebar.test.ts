import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import AppSidebar from './AppSidebar.svelte';

const status = { tone: 'idle' as const, label: 'Local archive ready', text: 'Local archive' };

function renderSidebar(overrides: Record<string, unknown> = {}) {
  const props = {
    active: 'everything' as const,
    collapsed: false,
    showCollapseToggle: true,
    status,
    onNavigate: vi.fn(),
    onToggleCollapsed: vi.fn(),
    onOpenShortcuts: vi.fn(),
    ...overrides
  };
  const view = render(AppSidebar, props);
  return { ...props, rerender: view.rerender, container: view.container };
}

describe('AppSidebar', () => {
  it('lists workspaces in People, Archive, Manage order with the active one current', () => {
    renderSidebar();
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(
      within(nav)
        .getAllByRole('button')
        .map((b) => b.getAttribute('aria-label') ?? b.textContent?.trim())
    ).toEqual([
      'Relationships',
      'Directory',
      'Reviews',
      'Everything',
      'Files',
      'Saved views',
      'Sources',
      'Operations',
      'Deletions',
      'Settings'
    ]);
    expect(within(nav).getByRole('button', { name: 'Everything' }).getAttribute('aria-current')).toBe(
      'page'
    );
    expect(within(nav).getByRole('button', { name: 'Files' }).hasAttribute('aria-current')).toBe(
      false
    );
  });

  it('navigates when an item is chosen', async () => {
    const props = renderSidebar();
    await fireEvent.click(screen.getByRole('button', { name: 'Deletions' }));
    expect(props.onNavigate).toHaveBeenCalledWith('deletions');
  });

  it('keeps full accessible names in the icon rail and hides group headings', () => {
    renderSidebar({ collapsed: true });
    expect(screen.getByRole('button', { name: 'Saved views' })).toBeTruthy();
    expect(screen.queryByText('People')).toBeNull();
    expect(screen.getByRole('button', { name: 'Everything' }).getAttribute('aria-current')).toBe(
      'page'
    );
  });

  it('keeps every sidebar item inside its landmark and hides the decorative status dot', () => {
    const { container } = renderSidebar();
    const sidebar = screen.getByRole('complementary', { name: 'Sidebar' });
    expect(sidebar.textContent).toBe(container.textContent);
    expect(within(sidebar).getByText('msgvault')).toBeTruthy();
    expect(within(sidebar).getByRole('navigation', { name: 'Primary' })).toBeTruthy();
    const dot = container.querySelector('[aria-label="Local archive ready"]');
    expect(dot?.closest('[aria-hidden="true"]')).toBeTruthy();
  });

  it('gives assistive tech the full archive status in the rail', () => {
    renderSidebar({ collapsed: true });
    expect(screen.getByText('Local archive ready').classList.contains('kit-sr-only')).toBe(true);
    expect(screen.queryByText('Local archive')).toBeNull();
  });

  it('lets keyboard users open the archive status tooltip in the rail', async () => {
    renderSidebar({ collapsed: true });
    const trigger = screen.getByText('Local archive ready').closest('.kit-tooltip-trigger') as HTMLElement;
    expect(trigger.tabIndex).toBe(0);
    trigger.focus();
    expect((await screen.findByRole('tooltip')).textContent?.trim()).toBe('Local archive ready');
  });

  it('toggles the rail and opens shortcuts from the footer', async () => {
    const props = renderSidebar();
    await fireEvent.click(screen.getByRole('button', { name: 'Collapse sidebar' }));
    await fireEvent.click(screen.getByRole('button', { name: /Keyboard shortcuts/ }));
    expect(props.onToggleCollapsed).toHaveBeenCalled();
    expect(props.onOpenShortcuts).toHaveBeenCalled();
    expect(screen.getByText('Local archive')).toBeTruthy();
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
