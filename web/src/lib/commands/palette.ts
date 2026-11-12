import type { Component } from 'svelte';
import Filter from '@lucide/svelte/icons/filter';
import Group from '@lucide/svelte/icons/group';
import ArrowDownUp from '@lucide/svelte/icons/arrow-down-up';
import ArrowUpDown from '@lucide/svelte/icons/arrow-up-down';
import ListChecks from '@lucide/svelte/icons/list-checks';
import ListX from '@lucide/svelte/icons/list-x';
import Trash2 from '@lucide/svelte/icons/trash-2';
import Search from '@lucide/svelte/icons/search';
import Keyboard from '@lucide/svelte/icons/keyboard';
import type { CommandID } from './registry';

export const PALETTE_ACTIONS: { id: CommandID; icon: Component; label?: string }[] = [
  { id: 'open-filters', icon: Filter },
  { id: 'open-grouping', icon: Group, label: 'Group by…' },
  { id: 'change-sort', icon: ArrowDownUp },
  { id: 'reverse-sort', icon: ArrowUpDown },
  { id: 'select-visible', icon: ListChecks },
  { id: 'clear-selection', icon: ListX },
  { id: 'review-delete-selected', icon: Trash2 },
  { id: 'review-delete-matching', icon: Trash2 },
  { id: 'focus-search', icon: Search },
  { id: 'open-keyboard-help', icon: Keyboard },
];
