import type { PersonEnrichmentProviderSetting as ProviderSetting, PersonEnrichmentProviderUpdate as ProviderUpdate } from '../api/generated/models';
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

/**
 * Arranges daemon settings into the categories and sections the daemon
 * declares. Settings supplies its own category layout and selected labels.
 * Settings whose group the daemon did not declare are dropped.
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

/** The value a setting holds on the daemon, as the plain value a control edits. */
export function persistedValue(setting: SettingState): unknown {
  const value = setting.value;
  if (!value) return undefined;
  if ('string' in value) return value.string;
  if ('integer' in value) return value.integer;
  if ('number' in value) return value.number;
  if ('boolean' in value) return value.boolean;
  return value.strings;
}

/** A control's value in the typed form the daemon accepts. */
export function typedValue(setting: SettingState | undefined, value: unknown): SettingValue {
  switch (setting?.kind) {
    case 'boolean':
      return { boolean: Boolean(value) };
    case 'integer':
      return { integer: Number(value) };
    case 'number':
      return { number: Number(value) };
    case 'string_array':
      return { strings: Array.isArray(value) ? value.map(String) : [] };
    default:
      return { string: String(value ?? '') };
  }
}

export function sameValue(draft: SettingValue, persisted: SettingValue | undefined): boolean {
  return persisted !== undefined && JSON.stringify(draft) === JSON.stringify(persisted);
}

/** An emptied number field is a draft in progress, never a value: Number('') is 0. */
export function isIncompleteNumber(setting: SettingState | undefined, value: unknown): boolean {
  return value === '' && (setting?.kind === 'integer' || setting?.kind === 'number');
}

/** The editable text of a value: arrays join with commas, empty is ''. */
export function valueText(value: unknown): string {
  if (Array.isArray(value)) return value.join(', ');
  return value == null ? '' : String(value);
}

/** Whether a draft differs from what the daemon holds. */
export function isChange(setting: SettingState | undefined, value: unknown): boolean {
  if (!setting) return true;
  if (isIncompleteNumber(setting, value)) return true;
  return !sameValue(typedValue(setting, value), setting.value);
}

export function providerUpdate(value: ProviderSetting): ProviderUpdate {
  return {
    kind: value.kind,
    enabled: value.enabled,
    endpoint: value.endpoint,
    poll_endpoint: value.poll_endpoint,
    mode: value.mode,
    tier: value.tier,
    num_results: value.num_results,
    allowed_identifiers: value.allowed_identifiers ?? [],
    target_keys: value.target_keys ?? [],
    allow_sensitive_targets: value.allow_sensitive_targets,
    retention_posture: value.retention_posture,
    training_posture: value.training_posture,
    refresh_interval: value.refresh_interval,
    request_timeout: value.request_timeout,
    poll_interval: value.poll_interval,
    max_job_age: value.max_job_age,
    max_retries: value.max_retries,
    max_requests_per_run: value.max_requests_per_run,
    max_requests_per_day: value.max_requests_per_day,
  };
}

export function readOnlyValue(setting: SettingState): string {
  if (setting.kind === 'secret') return setting.secret?.configured ? setting.secret.hint || 'Set' : 'None';
  const value = valueText(persistedValue(setting));
  const off = setting.validation?.off;
  if (off && value === (off.value ?? '')) return off.label;
  return value || 'Not set';
}
