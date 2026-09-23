import {
  checkSettingsPeopleInferenceProvider,
  consentSettingsPeopleInferenceProvider,
  deleteSettingsPeopleInferenceProvider,
  disableSettingsPeopleInference,
  getSettingsPeopleInference,
  putSettingsPeopleInferenceKey,
  putSettingsPeopleInferencePreset,
  putSettingsPeopleCodexProfile,
  revokeSettingsPeopleInferenceProvider,
  selectSettingsPeopleInference,
} from '../api/generated/api/api';
import type {
  PeopleInferenceCheckResponse,
  PeopleInferenceConsentRequest,
  PeopleInferenceKeyWriteRequest,
  PeopleInferencePresetCreateRequest,
  PeopleInferenceSelectionRequest,
  PeopleInferenceSettingsResponse,
  PeopleCodexProfileRequest,
} from '../api/generated/models';
import type { APIClient } from '../api/client';

export class PeopleInferenceConflictError extends Error {
  constructor() {
    super('People sweep settings changed on disk. Reload and review your changes before trying again.');
    this.name = 'PeopleInferenceConflictError';
  }
}

/** Committed generated settings operations share the server's config revision. */
export class PeopleInferenceAPI {
  private configETag = '';
  private status?: PeopleInferenceSettingsResponse;
  private invalidCredentialRevisions = new Set<string>();

  constructor(private readonly client: APIClient) {}

  async load(): Promise<PeopleInferenceSettingsResponse> {
    const { data, error, response } = await getSettingsPeopleInference(this.client);
    if (!data) throw new Error(errorMessage(error, 'Unable to load people sweep settings.'));
    this.configETag = '';
    this.status = undefined;
    const etag = response.headers.get('ETag');
    if (!etag) throw new Error('People sweep settings response omitted its config ETag. Reload before editing.');
    this.configETag = etag;
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async createPreset(name: string, request: PeopleInferencePresetCreateRequest): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    const { data, error, response } = await putSettingsPeopleInferencePreset({ name }, request, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to create people sweep profile.'));
    this.configETag = response.headers.get('ETag') ?? '';
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async createCodexProfile(sessionID: string, request: PeopleCodexProfileRequest): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    const { data, error, response } = await putSettingsPeopleCodexProfile({ id: sessionID }, request, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to create Codex profile.'));
    this.configETag = response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('Codex profile response omitted its config ETag. Reload before editing.');
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async setKey(name: string, value: string): Promise<PeopleInferenceSettingsResponse> {
    const revision = this.status?.profiles.find((profile) => profile.name === name)?.credential_revision;
    if (!revision || this.invalidCredentialRevisions.has(name)) {
      throw new Error('Reload people sweep settings to get the current profile credential revision.');
    }
    const request = { value } satisfies PeopleInferenceKeyWriteRequest;
    const { data, error, response } = await putSettingsPeopleInferenceKey({ name }, request, {
      ...this.client,
      headers: { 'If-Match': revision },
    });
    if (response.status === 412) {
      this.invalidCredentialRevisions.add(name);
      throw new Error('People provider credential changed. Reload people sweep settings before replacing its key.');
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to save people provider key.'));
    this.configETag = response.headers.get('ETag') ?? '';
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async check(name: string): Promise<PeopleInferenceCheckResponse> {
    const etag = this.requiredETag();
    const fingerprint = this.status?.profiles.find((profile) => profile.name === name)?.fingerprint;
    if (!fingerprint) throw new Error('Reload people sweep settings to check the current profile.');
    const { data, error, response } = await checkSettingsPeopleInferenceProvider({ name }, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Synthetic provider check failed.'));
    if (!data.ok || data.fingerprint !== fingerprint) {
      this.configETag = '';
      throw new Error('Synthetic check did not match the current profile. Reload and check again.');
    }
    return data;
  }

  async consent(name: string, fingerprint: string, confirmed: boolean): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    if (!confirmed) throw new Error('Confirm the exact disclosure before granting consent.');
    if (!fingerprint || this.status?.profiles.find((profile) => profile.name === name)?.fingerprint !== fingerprint) {
      throw new Error('Checked fingerprint no longer matches the current profile. Reload and check again.');
    }
    const request = { fingerprint, confirmed } satisfies PeopleInferenceConsentRequest;
    const { data, error, response } = await consentSettingsPeopleInferenceProvider({ name }, request, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to grant people inference consent.'));
    const profile = data.profiles.find((item) => item.name === name);
    if (profile?.fingerprint !== fingerprint || !profile.consent_active) {
      this.configETag = '';
      throw new Error('Consent was not recorded for the checked profile. Reload and try again.');
    }
    this.configETag = response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('Consent response omitted its config ETag. Reload before editing.');
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async revoke(name: string, fingerprint: string): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    if (!fingerprint || this.status?.profiles.find((profile) => profile.name === name)?.fingerprint !== fingerprint) {
      throw new Error('Profile fingerprint changed. Reload before revoking consent.');
    }
    const { data, error, response } = await revokeSettingsPeopleInferenceProvider({ name }, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to revoke people inference consent.'));
    const profile = data.profiles.find((item) => item.name === name);
    if (profile?.fingerprint !== fingerprint || profile.consent_active) {
      this.configETag = '';
      throw new Error('Consent revocation was not reflected in provider status. Reload and try again.');
    }
    this.configETag = response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('Consent revocation response omitted its config ETag. Reload before editing.');
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async disable(): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    const { data, error, response } = await disableSettingsPeopleInference({
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to disable people inference.'));
    if (data.configured_enabled) {
      this.configETag = '';
      throw new Error('People inference is still enabled in provider status. Reload and try again.');
    }
    this.configETag = response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('Disable response omitted its config ETag. Reload before editing.');
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async remove(name: string): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    if (!this.status?.profiles.some((profile) => profile.name === name)) {
      throw new Error('Reload people sweep settings to remove the current profile.');
    }
    const { data, error, response } = await deleteSettingsPeopleInferenceProvider({ name }, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to remove people inference profile.'));
    if (data.profiles.some((profile) => profile.name === name)) {
      this.configETag = '';
      throw new Error('Removed profile remains in provider status. Reload and try again.');
    }
    this.configETag = response.headers.get('ETag') ?? '';
    if (!this.configETag) throw new Error('Removal response omitted its config ETag. Reload before editing.');
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  async select(name: string): Promise<PeopleInferenceSettingsResponse> {
    const etag = this.requiredETag();
    const request = { name } satisfies PeopleInferenceSelectionRequest;
    const { data, error, response } = await selectSettingsPeopleInference(request, {
      ...this.client,
      headers: { 'If-Match': etag },
    });
    if (response.status === 412) {
      this.configETag = '';
      throw new PeopleInferenceConflictError();
    }
    if (!data) throw new Error(errorMessage(error, 'Unable to select people sweep profile.'));
    this.configETag = response.headers.get('ETag') ?? '';
    this.status = data;
    this.invalidCredentialRevisions.clear();
    return data;
  }

  private requiredETag(): string {
    if (!this.configETag) throw new Error('Load people sweep settings before editing.');
    return this.configETag;
  }
}

function errorMessage(error: unknown, fallback: string): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') {
    return error.message;
  }
  return fallback;
}
