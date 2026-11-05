import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import {
  COMMAND_DEFINITIONS,
  createCommandRegistry,
  type CommandHandlers,
  type CommandID
} from '../../commands/registry';
import KeyboardHelp from './KeyboardHelp.svelte';

describe('keyboard command registry', () => {
  it('covers every documented keyboard command with a handler', () => {
    const handlers = handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id));
    const registry = createCommandRegistry(handlers);

    expect(registry.map(({ id }) => id)).toEqual(COMMAND_DEFINITIONS.map(({ id }) => id));
    expect(registry.find(({ id }) => id === 'select-visible')?.keys).toEqual(['a']);
    expect(registry.find(({ id }) => id === 'select-visible')?.combos).toEqual(['a', 'shift+a']);
    expect(registry.find(({ id }) => id === 'clear-selection')?.keys).toContain('x');
    expect(registry.some(({ keys }) => keys.includes('Tab'))).toBe(false);
    expect(registry.filter(({ destructive }) => destructive).every(({ review }) => review)).toBe(true);
  });

  it('fails closed when a documented command lacks a handler', () => {
    const ids = COMMAND_DEFINITIONS.map(({ id }) => id).filter((id) => id !== 'clear-selection');
    expect(() => createCommandRegistry(handlersFor(ids))).toThrow(/clear-selection.*handler/i);
  });

  it('searches selection commands and opens the palette from the footer', async () => {
    const onclose = vi.fn();
    const onOpenCommands = vi.fn();
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onclose,
      onOpenCommands
    });

    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeDefined();
    expect(screen.getByText('Select all visible rows')).toBeDefined();
    expect(screen.getByText('Clear selection')).toBeDefined();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' }), {
      target: { value: 'selection' }
    });
    expect(screen.getByText('Select all visible rows')).toBeDefined();
    expect(screen.queryByText('Open filters')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: /^Open command palette/ }));
    expect(onOpenCommands).toHaveBeenCalledExactlyOnceWith('');
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onclose).toHaveBeenCalledOnce();
  });

  it('renders a modified shortcut as one chord instead of key alternatives', () => {
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onOpenCommands: vi.fn(),
      onclose: vi.fn()
    });

    const command = screen.getByText('Open command palette', { selector: 'dt' }).closest('div');
    expect(command).not.toBeNull();
    expect(command?.querySelector('[aria-label="Ctrl K"]')).not.toBeNull();
    expect(command?.querySelectorAll('.kit-kbd-badge__key')).toHaveLength(2);
    expect(command?.textContent).not.toContain('or');
  });

  it.each([
    ['shift', 'Extend selection to focused row'],
    ['enter', 'Open or drill into focused row'],
    ['ctrl', 'Open command palette'],
    ['cmd', 'Open command palette']
  ])('finds shortcuts by the readable key name %s', async (query, label) => {
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onOpenCommands: vi.fn(),
      onclose: vi.fn()
    });
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' }), {
      target: { value: query }
    });
    expect(screen.getByText(label, { selector: 'dt' })).toBeDefined();
    expect(screen.queryByText('Open filters')).toBeNull();
  });

  it('shows review chords and offers the query to command search when no shortcut matches', async () => {
    const onOpenCommands = vi.fn();
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onOpenCommands,
      onclose: vi.fn()
    });

    const selected = screen.getByText('Review selected messages for deletion').closest('div');
    const matching = screen.getByText('Review all matching messages for deletion').closest('div');
    expect(selected?.querySelector('[aria-label="D"]')).not.toBeNull();
    expect(matching?.querySelector('[aria-label="Shift D"]')).not.toBeNull();
    expect(screen.getAllByText('opens review first')).toHaveLength(2);

    const search = screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' });
    await fireEvent.input(search, { target: { value: 'Ctrl K' } });
    expect(screen.getByText('Open command palette', { selector: 'dt' })).toBeDefined();
    expect(screen.queryByText('Open filters')).toBeNull();
    await fireEvent.input(search, { target: { value: 'zzzxxyy' } });
    expect(screen.getByText('No shortcuts match "zzzxxyy"')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Search commands instead' }));
    expect(onOpenCommands).toHaveBeenCalledExactlyOnceWith('zzzxxyy');
  });
});

function handlersFor(ids: CommandID[]): CommandHandlers {
  return Object.fromEntries(ids.map((id) => [id, vi.fn()])) as unknown as CommandHandlers;
}
