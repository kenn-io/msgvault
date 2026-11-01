import type { Component } from 'svelte';
import Inbox from '@lucide/svelte/icons/inbox';
import Paperclip from '@lucide/svelte/icons/paperclip';
import Plug from '@lucide/svelte/icons/plug';
import Settings from '@lucide/svelte/icons/settings';
import Trash2 from '@lucide/svelte/icons/trash-2';
import Users from '@lucide/svelte/icons/users';

import type { ExploreWorkspace } from '../../explore/models';

export interface NavigationItem {
  id: ExploreWorkspace;
  label: string;
  icon: Component;
}

export const SIDEBAR_COLLAPSED_KEY = 'msgvault.sidebar.collapsed';

/** The places a user goes to. Saved views nest under Messages and Files. */
export const NAVIGATION_ITEMS: NavigationItem[] = [
  { id: 'everything', label: 'Messages', icon: Inbox },
  { id: 'relationships', label: 'People', icon: Users },
  { id: 'files', label: 'Files', icon: Paperclip },
  { id: 'sources', label: 'Sources', icon: Plug },
];

export const DELETIONS_ITEM: NavigationItem = { id: 'deletions', label: 'Staged deletions', icon: Trash2 };

export const SETTINGS_ITEM: NavigationItem = { id: 'settings', label: 'Settings', icon: Settings };

export function workspaceLabel(id: ExploreWorkspace): string {
  if (id === 'directory') return 'Directory';
  if (id === 'directory_review') return 'Reviews';
  if (id === 'deletions') return 'Staged deletions';
  return [...NAVIGATION_ITEMS, SETTINGS_ITEM].find((item) => item.id === id)?.label ?? 'msgvault';
}

export function relationshipDestination(participantID: number) {
  return {
    workspace: 'relationships' as const,
    query: '',
    relationshipFacet: 'people' as const,
    relationshipTarget: `cluster:${participantID}`,
    directoryPersonID: null,
    relationshipFiles: false,
    relationshipShowAll: false,
    analysisTarget: null,
    selectedIdentifier: null,
    selectedRow: null,
    activeRow: null,
    conversationAnchor: null,
    scrollAnchor: null,
  };
}

export function tasksSettingsDestination() {
  return {
    workspace: 'settings' as const,
    settingsCategory: 'tasks' as const,
    settingsAuthority: '' as const,
    selectedRow: null,
    conversationAnchor: null,
  };
}
