import type { Component } from 'svelte';
import Activity from '@lucide/svelte/icons/activity';
import Bookmark from '@lucide/svelte/icons/bookmark';
import CheckCheck from '@lucide/svelte/icons/check-check';
import Contact from '@lucide/svelte/icons/contact';
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

export interface NavigationGroup {
  label: string;
  items: NavigationItem[];
}

export const SIDEBAR_COLLAPSED_KEY = 'msgvault.sidebar.collapsed';

export const NAVIGATION_GROUPS: NavigationGroup[] = [
  {
    label: 'People',
    items: [
      { id: 'relationships', label: 'Relationships', icon: Users },
      { id: 'directory', label: 'Directory', icon: Contact },
      { id: 'directory_review', label: 'Reviews', icon: CheckCheck }
    ]
  },
  {
    label: 'Archive',
    items: [
      { id: 'everything', label: 'Everything', icon: Inbox },
      { id: 'files', label: 'Files', icon: Paperclip },
      { id: 'saved_views', label: 'Saved views', icon: Bookmark }
    ]
  },
  {
    label: 'Manage',
    items: [
      { id: 'sources', label: 'Sources', icon: Plug },
      { id: 'operations', label: 'Operations', icon: Activity },
      { id: 'deletions', label: 'Deletions', icon: Trash2 },
      { id: 'settings', label: 'Settings', icon: Settings }
    ]
  }
];

export function workspaceLabel(id: ExploreWorkspace): string {
  for (const group of NAVIGATION_GROUPS) {
    const item = group.items.find((candidate) => candidate.id === id);
    if (item) return item.label;
  }
  return 'msgvault';
}
