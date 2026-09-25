import type { APIClient } from '../api/client';
import type { APIResponse } from '../api/runtime';
import {
  checkSettingsPeopleInferenceProvider,
  consentSettingsPeopleInferenceProvider,
  deleteSettingsPeopleInferenceProvider,
  disableSettingsPeopleInference,
  getSettingsPeopleInference,
  putSettingsPeopleInferenceKey,
  putSettingsPeopleInferencePreset,
  revokeSettingsPeopleInferenceProvider,
  selectSettingsPeopleInference,
} from '../api/generated/api/api';
import type {
  PeopleInferencePresetCreateRequest,
  PeopleInferenceProfileSetting,
  PeopleInferenceSettingsResponse,
} from '../api/generated/models';

export class PeopleInferenceController {
  status = $state<PeopleInferenceSettingsResponse>();
  selectedName = $state('');
  loading = $state(true);
  busy = $state(false);
  error = $state('');
  checkedProfile = $state<PeopleInferenceProfileSetting>();
  disclosureConfirmed = $state(false);
  private configETag = '';
  private consentedFingerprint = $state('');
  private destroyed = false;
  private selectionEpoch = 0;

  constructor(private readonly client: APIClient) {}

  get selectedProfile(): PeopleInferenceProfileSetting | undefined {
    return this.status?.profiles.find((profile) => profile.name === this.selectedName);
  }

  get canConsent(): boolean {
    return Boolean(this.disclosureConfirmed && this.checkedProfile?.fingerprint &&
      this.checkedProfile.fingerprint === this.selectedProfile?.fingerprint);
  }

  get canSelect(): boolean {
    return Boolean(this.checkedProfile?.fingerprint &&
      this.checkedProfile.fingerprint === this.selectedProfile?.fingerprint &&
      this.selectedProfile?.consent_active && this.consentedFingerprint === this.checkedProfile.fingerprint);
  }

  async load(): Promise<void> {
    this.loading = true;
    this.clearCheck();
    await this.run(async () => {
      this.capture(await getSettingsPeopleInference(this.client));
      if (!this.status?.profiles.some((profile) => profile.name === this.selectedName)) {
        this.choose(this.status?.configured_name ?? this.status?.profiles[0]?.name ?? '');
      }
    });
    if (!this.destroyed) this.loading = false;
  }

  choose(name: string): void {
    this.selectionEpoch += 1;
    this.selectedName = name;
    this.clearCheck();
  }

  async create(name: string, request: PeopleInferencePresetCreateRequest, key: string): Promise<void> {
    await this.run(async () => {
      this.capture(await putSettingsPeopleInferencePreset({ name }, request, this.configOptions()));
      if (this.destroyed) return;
      // Creating a profile and storing its key are separate writes. Keep the
      // saved profile visible so a failed key write can be retried in place.
      this.choose(name);
      if (key) await this.writeKey(name, key);
    });
  }

  async saveKey(key: string): Promise<void> {
    if (!this.selectedName || !key) return;
    this.clearCheck();
    await this.run(() => this.writeKey(this.selectedName, key));
  }

  private async writeKey(name: string, key: string): Promise<void> {
    const profile = this.status?.profiles.find((item) => item.name === name);
    if (!profile?.credential_revision) throw new Error('Reload people sweep settings before replacing the key.');
    const result = await putSettingsPeopleInferenceKey({ name }, { value: key }, {
      ...this.client, headers: { 'If-Match': profile.credential_revision },
    });
    if (result.response.status === 412) {
      profile.credential_revision = undefined;
      throw new Error('People provider credential changed. Reload people sweep settings before replacing its key.');
    }
    this.capture(result);
  }

  async check(): Promise<void> {
    const profile = this.selectedProfile;
    const selectionEpoch = this.selectionEpoch;
    if (!profile) return;
    this.clearCheck();
    await this.run(async () => {
      const result = this.read(await checkSettingsPeopleInferenceProvider({ name: profile.name }, this.configOptions()));
      const refreshed = await getSettingsPeopleInference(this.client);
      this.capture(refreshed);
      if (this.destroyed || this.selectionEpoch !== selectionEpoch) return;
      if (!result.ok || !result.fingerprint || result.fingerprint !== profile.fingerprint ||
        result.fingerprint !== this.selectedProfile?.fingerprint) {
        throw new Error('This profile changed after the check. Reload and check again.');
      }
      this.checkedProfile = this.selectedProfile;
    });
  }

  async consent(): Promise<void> {
    if (!this.canConsent) return;
    const name = this.selectedName;
    const fingerprint = this.checkedProfile!.fingerprint!;
    await this.run(async () => {
      this.capture(await consentSettingsPeopleInferenceProvider({ name }, {
        fingerprint, confirmed: true,
      }, this.configOptions()));
      const profile = this.status?.profiles.find((item) => item.name === name);
      if (profile?.fingerprint !== fingerprint || !profile.consent_active) {
        this.clearCheck();
        throw new Error('Consent was not recorded for the checked profile. Reload and try again.');
      }
      this.consentedFingerprint = fingerprint;
    });
  }

  async select(): Promise<void> {
    if (!this.canSelect) return;
    await this.run(async () => {
      this.capture(await selectSettingsPeopleInference({ name: this.selectedName }, this.configOptions()));
    });
  }

  async revoke(): Promise<void> {
    if (!this.selectedProfile?.consent_active) return;
    this.clearCheck();
    await this.run(async () => {
      this.capture(await revokeSettingsPeopleInferenceProvider({ name: this.selectedName }, this.configOptions()));
    });
  }

  async disable(): Promise<void> {
    this.clearCheck();
    await this.run(async () => {
      this.capture(await disableSettingsPeopleInference(this.configOptions()));
    });
  }

  async remove(name: string): Promise<void> {
    this.clearCheck();
    await this.run(async () => {
      this.capture(await deleteSettingsPeopleInferenceProvider({ name }, this.configOptions()));
      if (!this.destroyed) this.choose(this.status?.profiles[0]?.name ?? '');
    });
  }

  destroy(): void { this.destroyed = true; }

  private clearCheck(): void {
    this.checkedProfile = undefined;
    this.disclosureConfirmed = false;
    this.consentedFingerprint = '';
  }

  private configOptions() {
    if (!this.configETag) throw new Error('Reload people sweep settings before editing.');
    return { ...this.client, headers: { 'If-Match': this.configETag } };
  }

  private read<T>(result: APIResponse<T>): T {
    if (result.response.status === 412) {
      this.configETag = '';
      this.clearCheck();
      throw new Error('People sweep settings changed on disk. Reload and review your changes before trying again.');
    }
    if (!result.data) throw new Error(result.error?.message || 'Unable to update people sweep settings.');
    return result.data;
  }

  private capture(result: APIResponse<PeopleInferenceSettingsResponse>): void {
    const status = this.read(result);
    if (this.destroyed) return;
    this.status = status;
    this.configETag = result.response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('People sweep settings response omitted its config ETag. Reload before editing.');
  }

  private async run(action: () => Promise<void>): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    this.error = '';
    try { await action(); }
    catch (cause) {
      if (!this.destroyed) this.error = cause instanceof Error ? cause.message : 'Unable to update people sweep settings.';
    } finally {
      if (!this.destroyed) this.busy = false;
    }
  }
}

export function credentialStatus(profile: PeopleInferenceProfileSetting): string {
  if (profile.protocol === 'codex_app_server') return 'Codex is unavailable in this release';
  if (profile.credential_source === 'env') {
    const variable = profile.credential_env || 'the provider variable';
    return profile.credential_configured ? `Environment ${variable} ready` : `Set ${variable} on daemon host`;
  }
  return profile.credential_configured ? 'Stored key' : 'Key needed';
}
