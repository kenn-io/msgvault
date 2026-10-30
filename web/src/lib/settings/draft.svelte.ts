import { getSettings, patchSettings } from '../api/generated/api/api';
import type { SettingsResponse, SettingUpdate } from '../api/generated/models';
import type { APIClient } from '../api/client';
import {
  isChange,
  isIncompleteNumber,
  persistedValue,
  typedValue,
  type SettingState,
} from './catalog';

export function draftValue(settings: SettingState[], drafts: Record<string, unknown>, key: string): unknown {
  if (Object.hasOwn(drafts, key)) return drafts[key];
  const setting = settings.find(setting => setting.key === key);
  return setting ? persistedValue(setting) : undefined;
}

export function editDraft(settings: SettingState[], drafts: Record<string, unknown>, key: string, value: unknown): Record<string, unknown> {
  const next = { ...drafts };
  const setting = settings.find(setting => setting.key === key);
  if (setting && !isChange(setting, value)) delete next[key];
  else next[key] = value;
  return next;
}

export function scalarUpdates(settings: SettingState[], drafts: Record<string, unknown>) {
  return Object.entries(drafts).filter(([key]) => !settings.find(setting => setting.key === key)?.read_only).map(([key, value]) => ({ key, value: typedValue(settings.find(setting => setting.key === key), value) }));
}

/** Loads the settings catalog and keeps edits until the daemon accepts them. */
export class SettingsDraft {
  settings = $state<SettingState[]>([]);
  drafts = $state<Record<string, unknown>>({});
  loading = $state(true);
  saving = $state(false);
  pendingRestart = $state(false);
  error = $state('');
  etag = $state('');
  credentialETag = $state('');
  document = $state<SettingsResponse>();
  conflicted = false;
  #client: APIClient;
  #controller?: AbortController;

  constructor(client: APIClient) {
    this.#client = client;
  }

  get dirty(): boolean {
    return Object.keys(this.drafts).length > 0;
  }

  /** A number field the user emptied: it blocks saving until a number is back. */
  get incomplete(): boolean {
    return Object.entries(this.drafts).some(([key, value]) => isIncompleteNumber(this.find(key), value));
  }

  find(key: string): SettingState | undefined {
    return this.settings.find((setting) => setting.key === key);
  }

  value(key: string): unknown {
    return draftValue(this.settings, this.drafts, key);
  }

  set(key: string, value: unknown): void {
    this.drafts = editDraft(this.settings, this.drafts, key, value);
  }

  discard(): void {
    this.drafts = {};
    this.error = '';
  }

  async load(keepDrafts = true): Promise<boolean> {
    this.#controller?.abort();
    const controller = (this.#controller = new AbortController());
    if (!keepDrafts) this.loading = true;
    try {
      const { data, response, error } = await getSettings({ ...this.#client, signal: controller.signal });
      if (controller.signal.aborted) return false;
      if (!data) throw new Error(errorMessage(error, "Can't load settings."));
      this.accept(data, response.headers);
      this.drafts = keepDrafts ? Object.fromEntries(Object.entries(this.drafts).filter(([key, value]) => isChange(this.find(key), value))) : {};
      this.error = '';
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.error = cause instanceof Error ? cause.message : "Can't load settings.";
      return false;
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  /** Resolves true when the daemon accepted every change. */
  async save(extra: SettingUpdate[] = []): Promise<boolean> {
    const updates = [...scalarUpdates(this.settings, this.drafts), ...extra];
    if (updates.length === 0 || this.incomplete) return false;
    this.conflicted = false;
    this.saving = true;
    this.error = '';
    try {
      const { data, response, error } = await patchSettings(
        { updates },
        { ...this.#client, headers: { 'If-Match': this.etag } },
      );
      if (response.status === 412) {
        await this.load(true);
        this.conflicted = true;
        this.error = 'The settings changed on disk. The latest are loaded. Review your changes and save again.';
        return false;
      }
      if (!data) {
        this.error = errorMessage(error, "Can't save settings.");
        return false;
      }
      this.accept(data, response.headers);
      this.drafts = {};
      return true;
    } catch (cause) {
      this.error = cause instanceof Error ? cause.message : "Can't save settings.";
      return false;
    } finally {
      this.saving = false;
    }
  }

  private accept(data: SettingsResponse, headers: Headers): void {
    this.document = data;
    this.settings = data.settings;
    this.pendingRestart = data.pending_restart;
    this.etag = headers.get('ETag') ?? this.etag;
    this.credentialETag = headers.get('Credential-ETag') ?? data.credential_etag ?? this.credentialETag;
  }

  destroy(): void {
    this.#controller?.abort();
  }
}

function errorMessage(value: unknown, fallback: string): string {
  return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string' && value.message ? value.message : fallback;
}
