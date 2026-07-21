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
    expect(registry.find(({ id }) => id === 'select-visible')?.keys).toContain('A');
    expect(registry.find(({ id }) => id === 'clear-selection')?.keys).toContain('x');
    expect(registry.some(({ keys }) => keys.includes('Tab'))).toBe(false);
    expect(registry.filter(({ destructive }) => destructive).every(({ review }) => review)).toBe(true);
  });

  it('fails closed when a documented command lacks a handler', () => {
    const ids = COMMAND_DEFINITIONS.map(({ id }) => id).filter((id) => id !== 'clear-selection');
    expect(() => createCommandRegistry(handlersFor(ids))).toThrow(/clear-selection.*handler/i);
  });

  it('renders and searches the registry, including selection commands', async () => {
    const onclose = vi.fn();
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onclose
    });

    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeDefined();
    expect(screen.getByText('Select all visible rows')).toBeDefined();
    expect(screen.getByText('Clear selection')).toBeDefined();
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search keyboard shortcuts' }), {
      target: { value: 'selection' }
    });
    expect(screen.getByText('Select all visible rows')).toBeDefined();
    expect(screen.queryByText('Open filters')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onclose).toHaveBeenCalledOnce();
  });

  it('renders a modified shortcut as one chord instead of key alternatives', () => {
    render(KeyboardHelp, {
      commands: createCommandRegistry(handlersFor(COMMAND_DEFINITIONS.map(({ id }) => id))),
      onclose: vi.fn()
    });

    const command = screen.getByText('Open command palette').closest('div');
    expect(command).not.toBeNull();
    expect(command?.querySelector('[aria-label="Mod K"]')).not.toBeNull();
    expect(command?.textContent).not.toContain('or');
  });
});

function handlersFor(ids: CommandID[]): CommandHandlers {
  return Object.fromEntries(ids.map((id) => [id, vi.fn()])) as unknown as CommandHandlers;
}
