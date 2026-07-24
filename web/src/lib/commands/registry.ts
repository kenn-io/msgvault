export type CommandHandler = (event?: KeyboardEvent) => void;

export const COMMAND_DEFINITIONS = [
  command('move-next', 'Move to next row', ['J', '↓'], ['j', 'arrowdown'], 'Navigate'),
  command('move-previous', 'Move to previous row', ['K', '↑'], ['k', 'arrowup'], 'Navigate'),
  command('reader-previous', 'Previous item in reader', ['H'], ['h'], 'Navigate'),
  command('reader-next', 'Next item in reader', ['L'], ['l'], 'Navigate'),
  command('page-up', 'Move up one page', ['PgUp'], ['pageup'], 'Navigate'),
  command('page-down', 'Move down one page', ['PgDn'], ['pagedown'], 'Navigate'),
  command('first-row', 'Move to first row', ['Home'], ['home'], 'Navigate'),
  command('last-row', 'Move to last row', ['End'], ['end'], 'Navigate'),
  command('open-row', 'Open or drill into focused row', ['Enter'], ['enter'], 'Navigate'),
  command('close-layer', 'Close current layer or restore context', ['Esc'], ['escape'], 'Navigate'),
  command('focus-search', 'Focus search', ['/'], ['/'], 'Navigate'),
  command('toggle-selection', 'Toggle focused row selection', ['Space'], ['space'], 'Selection'),
  command('select-visible', 'Select all visible rows', ['A'], ['shift+a'], 'Selection'),
  command('clear-selection', 'Clear selection', ['x'], ['x'], 'Selection'),
  command('review-delete-selected', 'Review selected messages for deletion', ['d'], ['d'], 'Safety', true),
  command('review-delete-matching', 'Review all matching messages for deletion', ['D'], ['shift+d'], 'Safety', true),
  command('open-filters', 'Open filters', ['F'], ['f'], 'Analyze'),
  command('open-grouping', 'Open grouping controls', ['G'], ['g'], 'Analyze'),
  command('change-sort', 'Change sort', ['S'], ['s'], 'Analyze'),
  command('reverse-sort', 'Reverse sort direction', ['R'], ['r'], 'Analyze'),
  command('open-keyboard-help', 'Open searchable keyboard help', ['?'], ['shift+/'], 'Help'),
  command('open-command-palette', 'Open command palette', ['Mod', 'K'], ['mod+k'], 'Help')
] as const;

export type CommandID = typeof COMMAND_DEFINITIONS[number]['id'];
export type CommandHandlers = Record<CommandID, CommandHandler>;

export interface CommandDefinition {
  id: string;
  label: string;
  keys: readonly string[];
  combos: readonly string[];
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
  destructive = false
) {
  return {
    id,
    label,
    keys,
    combos,
    section,
    keywords: `${section} ${label} ${keys.join(' ')}`,
    destructive,
    review: destructive
  } as const;
}
