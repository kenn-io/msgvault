const SETTINGS_NAVIGATION_TARGETS = {
  document_index: {
    authority: 'document_index', categoryID: 'archive', settingKey: 'analytics.auto_build_cache'
  },
  document_vector: {
    authority: 'document_vector', categoryID: 'search', settingKey: 'vector.enabled'
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
}
