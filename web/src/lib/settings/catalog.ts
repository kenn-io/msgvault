import type {
  SecretSettingState as GeneratedSecretSettingState,
  Setting as GeneratedSetting,
  SettingGroup as GeneratedSettingGroup,
  SettingSection as GeneratedSettingSection,
  SettingValue as GeneratedSettingValue,
  SettingsResponse as GeneratedSettingsResponse,
} from '../api/generated/models';

export type SettingState = GeneratedSetting;
export type SettingKind = SettingState['kind'];

export type SecretSettingState = GeneratedSecretSettingState;

export type SettingValue = GeneratedSettingValue;
export type SettingsDocument = GeneratedSettingsResponse;
export type SettingGroupState = GeneratedSettingGroup;
export type SettingSectionState = GeneratedSettingSection;

/** One titled run of settings inside a category, in daemon order. */
export interface SettingsSection {
  id: string;
  label: string;
  description: string;
  settings: SettingState[];
}

/** One settings category as the daemon describes it. `sections` is empty
 * when the daemon declares none; `settings` always holds the full list. */
export interface SettingsGroup {
  id: string;
  label: string;
  description: string;
  sections: SettingsSection[];
  settings: SettingState[];
}

/** How saved changes in a run of settings take effect. Read-only settings
 * cannot change here, so they do not count. */
export type RestartPosture = 'live' | 'restart' | 'mixed' | 'none';

/**
 * Arranges daemon settings into the categories and sections the daemon
 * declares. The daemon is the only source of labels, descriptions, order,
 * and grouping; settings whose group the daemon did not declare are dropped.
 */
export function groupSettings(
  settings: readonly SettingState[],
  daemonGroups: readonly SettingGroupState[],
): SettingsGroup[] {
  return daemonGroups
    .map((group) => {
      const members = settings.filter((setting) => setting.group === group.id);
      const sections = (group.sections ?? [])
        .map((section) => ({
          id: section.id,
          label: section.label,
          description: section.description ?? '',
          settings: members.filter((setting) => setting.section === section.id),
        }))
        .filter((section) => section.settings.length > 0);
      return { id: group.id, label: group.label, description: group.description, sections, settings: members };
    })
    .filter((group) => group.settings.length > 0);
}

export function restartPosture(settings: readonly SettingState[]): RestartPosture {
  const editable = settings.filter((setting) => !setting.read_only);
  if (editable.length === 0) return 'none';
  const restart = editable.filter((setting) => setting.restart_required).length;
  if (restart === 0) return 'live';
  if (restart === editable.length) return 'restart';
  return 'mixed';
}

export function hasHostManaged(settings: readonly SettingState[]): boolean {
  return settings.some((setting) => Boolean(setting.read_only));
}
