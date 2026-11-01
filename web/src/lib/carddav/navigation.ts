const SETTINGS_NAVIGATION_TARGETS = {
  semantic_search: {
    authority: 'semantic_search', categoryID: 'search', settingKey: 'vector.enabled'
  },
  person_embeddings: {
    authority: 'person_embeddings', categoryID: 'search', settingKey: 'vector.people.enabled'
  },
  visual_attachments: {
    authority: 'visual_attachments', categoryID: 'search', settingKey: 'vector.multimodal.enabled'
  }
} as const;

export type SettingsNavigationAuthority = keyof typeof SETTINGS_NAVIGATION_TARGETS;
export type SettingsNavigationTarget = (typeof SETTINGS_NAVIGATION_TARGETS)[SettingsNavigationAuthority];

export function normalizeSettingsNavigationAuthority(value: unknown): SettingsNavigationAuthority | '' {
  return typeof value === 'string' && Object.hasOwn(SETTINGS_NAVIGATION_TARGETS, value)
    ? value as SettingsNavigationAuthority
    : '';
}

export function settingsNavigationTarget(
  authority: SettingsNavigationAuthority | ''
): SettingsNavigationTarget | undefined {
  return authority === '' ? undefined : SETTINGS_NAVIGATION_TARGETS[authority];
}

export interface CardDAVSettingsRequest {
  key: number;
  conflictID?: number;
  connection?: string;
}
