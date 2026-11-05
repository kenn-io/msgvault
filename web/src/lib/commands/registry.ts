import { formatShortcutKeys } from '@kenn-io/kit-ui';

export type CommandHandler = (event?: KeyboardEvent) => void;

export const COMMAND_DEFINITIONS = [
  command('move-next', 'Move to next row', ['j', 'arrowdown'], ['j', 'arrowdown'], 'Navigate'),
  command('move-previous', 'Move to previous row', ['k', 'arrowup'], ['k', 'arrowup'], 'Navigate'),
  command('reader-previous', 'Previous item in reader', ['h'], ['h'], 'Navigate'),
  command('reader-next', 'Next item in reader', ['l'], ['l'], 'Navigate'),
  command('page-up', 'Move up one page', ['pageup'], ['pageup'], 'Navigate'),
  command('page-down', 'Move down one page', ['pagedown'], ['pagedown'], 'Navigate'),
  command('first-row', 'Move to first row', ['home'], ['home'], 'Navigate'),
  command('last-row', 'Move to last row', ['end'], ['end'], 'Navigate'),
  command('open-row', 'Open or drill into focused row', ['enter'], ['enter'], 'Navigate'),
  command('close-layer', 'Close current layer or restore context', ['escape'], ['escape'], 'Navigate'),
  command('focus-search', 'Focus search', ['/'], ['/'], 'Navigate', false, ['root', 'deletions']),
  command('toggle-selection', 'Toggle focused row selection', ['space'], ['space'], 'Selection'),
  command('extend-selection', 'Extend selection to focused row', ['shift+space'], ['shift+space'], 'Selection'),
  command('select-visible', 'Select all visible rows', ['a'], ['a', 'shift+a'], 'Selection'),
  command('clear-selection', 'Clear selection', ['x'], ['x'], 'Selection'),
  command('review-delete-selected', 'Review selected messages for deletion', ['d'], ['d'], 'Safety', true),
  command('review-delete-matching', 'Review all matching messages for deletion', ['shift+d'], ['shift+d'], 'Safety', true),
  command('open-filters', 'Open filters', ['f'], ['f'], 'Analyze'),
  command('open-grouping', 'Open grouping controls', ['g'], ['g'], 'Analyze'),
  command('change-sort', 'Change sort', ['s'], ['s'], 'Analyze'),
  command('reverse-sort', 'Show sort order', ['r'], ['r'], 'Analyze'),
  command('open-keyboard-help', 'Open searchable keyboard help', ['shift+/'], ['shift+/'], 'Help'),
  command('open-command-palette', 'Open command palette', ['mod+k'], ['mod+k'], 'Help')
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
  keys: readonly string[],
  combos: readonly string[],
  section: string,
  destructive = false,
  scopes: readonly string[] = ['root']
) {
  return {
    id,
    label,
    keys,
    combos,
    scopes,
    section,
    keywords: `${section} ${label} ${keys.map((combo) => formatShortcutKeys(combo).join(' ')).join(' ')}`,
    destructive,
    review: destructive
  } as const;
}
