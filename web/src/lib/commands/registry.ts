import { formatShortcutKeys } from '@kenn-io/kit-ui';

export type CommandHandler = (event?: KeyboardEvent) => void;

export const COMMAND_DEFINITIONS = [
  command('move-next', 'Move to next row', ['j', 'arrowdown'], 'Navigate'),
  command('move-previous', 'Move to previous row', ['k', 'arrowup'], 'Navigate'),
  command('reader-previous', 'Previous item in reader', ['h'], 'Navigate'),
  command('reader-next', 'Next item in reader', ['l'], 'Navigate'),
  command('page-up', 'Move up one page', ['pageup'], 'Navigate'),
  command('page-down', 'Move down one page', ['pagedown'], 'Navigate'),
  command('first-row', 'Move to first row', ['home'], 'Navigate'),
  command('last-row', 'Move to last row', ['end'], 'Navigate'),
  command('open-row', 'Open or drill into focused row', ['enter'], 'Navigate'),
  command('close-layer', 'Close current layer or restore context', ['escape'], 'Navigate'),
  command('focus-search', 'Focus search', ['/'], 'Navigate', false, ['root', 'deletions']),
  command('toggle-selection', 'Toggle focused row selection', ['space'], 'Selection'),
  command('extend-selection', 'Extend selection to focused row', ['shift+space'], 'Selection'),
  command('select-visible', 'Select all visible rows', ['a', 'shift+a'], 'Selection', false, ['root'], ['a']),
  command('clear-selection', 'Clear selection', ['x'], 'Selection'),
  command('review-delete-selected', 'Review selected messages for deletion', ['d'], 'Safety', true),
  command('review-delete-matching', 'Review all matching messages for deletion', ['shift+d'], 'Safety', true),
  command('open-filters', 'Open filters', ['f'], 'Analyze'),
  command('open-grouping', 'Open grouping controls', ['g'], 'Analyze'),
  command('change-sort', 'Change sort', ['s'], 'Analyze'),
  command('reverse-sort', 'Show sort order', ['r'], 'Analyze'),
  command('open-keyboard-help', 'Open searchable keyboard help', ['shift+/'], 'Help'),
  command('open-command-palette', 'Open command palette', ['mod+k'], 'Help')
] as const;

export type CommandID = typeof COMMAND_DEFINITIONS[number]['id'];
export type CommandHandlers = Record<CommandID, CommandHandler>;

export interface CommandDefinition {
  id: string;
  label: string;
  keys: readonly string[];
  combos: readonly string[];
  scopes: readonly string[];
  section: string;
  keywords: string;
  destructive: boolean;
  review: boolean;
  disabled?: boolean;
}

export interface AppCommand extends CommandDefinition {
  run: CommandHandler;
}

export function createCommandRegistry(handlers: CommandHandlers): AppCommand[] {
  return COMMAND_DEFINITIONS.map((definition) => {
    const handler = handlers[definition.id];
    if (typeof handler !== 'function') throw new Error(`Documented command ${definition.id} lacks a handler`);
    return { ...definition, run: handler };
  });
}

function command<ID extends string>(
  id: ID,
  label: string,
  combos: readonly string[],
  section: string,
  destructive = false,
  scopes: readonly string[] = ['root'],
  keys: readonly string[] = combos
) {
  return {
    id,
    label,
    keys,
    combos,
    scopes,
    section,
    keywords: `${section} ${label} ${keys.map((combo) => `${combo.replaceAll('+', ' ').replace('mod', 'ctrl cmd command')} ${formatShortcutKeys(combo).join(' ')}`).join(' ')}`,
    destructive,
    review: destructive
  } as const;
}
