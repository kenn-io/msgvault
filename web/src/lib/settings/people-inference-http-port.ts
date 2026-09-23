import type { APIClient } from '../api/client';
import { cancelSettingsPeopleCodexLogin, getSettingsPeopleCodexLogin,
  getSettingsPeopleCodexModels, startSettingsPeopleCodexLogin } from '../api/generated/api/api';
import type { PeopleInferencePresetCreateRequest, PeopleInferenceProfileSetting,
  PeopleInferenceSettingsResponse, PeopleCodexLoginRequest, PeopleCodexProfileRequest } from '../api/generated/models';
import { PeopleInferenceAPI } from './people-inference-api';
import type { PeopleInferenceDraft, PeopleInferencePort, PeopleProvider,
  PeopleInferenceStatus } from './people-inference-controller.svelte';

/** Bridges the committed generated HTTP operations to the Settings journey. */
export class PeopleInferenceHTTPPort implements PeopleInferencePort {
  readonly supportsCodexEnrollment = true;
  readonly supportsEnvironmentReference = true;
  private readonly api: PeopleInferenceAPI;
  private source?: PeopleInferenceSettingsResponse;
  private readonly loginSessions = new Map<string, string>();

  constructor(private readonly client: APIClient) { this.api = new PeopleInferenceAPI(client); }

  async load(): Promise<PeopleInferenceStatus> { return this.capture(await this.api.load()); }

  async create(draft: PeopleInferenceDraft): Promise<PeopleInferenceStatus> {
    if (draft.provider === 'codex') {
      const sessionID = this.loginSessions.get(draft.name);
      if (!sessionID || !draft.model || !draft.reasoningEffort) {
        throw new Error('Complete Codex sign-in and choose a model and reasoning effort before creating the profile.');
      }
      const request = {
        model: draft.model, reasoning_effort: draft.reasoningEffort,
        allowed_sources: draft.allowedSources, source_since: draft.sourceSince,
        source_until: draft.sourceUntil, allow_sensitive: draft.allowSensitive,
        retention_posture: draft.retentionPosture, training_posture: draft.trainingPosture,
      } satisfies PeopleCodexProfileRequest;
      const status = this.capture(await this.api.createCodexProfile(sessionID, request));
      this.loginSessions.delete(draft.name);
      return status;
    }
    if (draft.provider !== 'openai' && draft.provider !== 'openrouter' && draft.provider !== 'venice') {
      throw new Error('This provider is not supported by the server settings API.');
    }
    const request = {
      preset_id: draft.provider, model: draft.model, allowed_sources: draft.allowedSources,
      source_since: draft.sourceSince, source_until: draft.sourceUntil,
      allow_sensitive: draft.allowSensitive, retention_posture: draft.retentionPosture,
      training_posture: draft.trainingPosture,
      credential_env: draft.environmentVariable,
    } satisfies PeopleInferencePresetCreateRequest;
    return this.capture(await this.api.createPreset(draft.name, request));
  }

  async saveKey(name: string, key: string): Promise<PeopleInferenceStatus> {
    return this.capture(await this.api.setKey(name, key));
  }

  async startLogin(name: string) {
    const request = { name } satisfies PeopleCodexLoginRequest;
    const { data, error } = await startSettingsPeopleCodexLogin(request, this.client);
    if (!data) throw new Error(errorMessage(error, 'Unable to start Codex sign-in.'));
    this.loginSessions.set(name, data.session_id);
    return { sessionID: data.session_id, url: data.verification_url,
      code: data.user_code, localDeadline: data.local_deadline };
  }

  async pollLogin(sessionID: string, signal: AbortSignal) {
    const { data, error } = await getSettingsPeopleCodexLogin({ id: sessionID }, { ...this.client, signal });
    if (!data) throw new Error(errorMessage(error, 'Unable to check Codex sign-in.'));
    if (data.state === 'pending' || data.state === 'complete') return { state: data.state as 'pending' | 'complete' };
    return { state: 'failed' as const, message: data.state === 'cancelled' ?
      'Codex sign-in was cancelled.' : 'Codex sign-in failed.' };
  }

  async cancelLogin(sessionID: string): Promise<void> {
    const { data, error } = await cancelSettingsPeopleCodexLogin({ id: sessionID }, this.client);
    if (!data) throw new Error(errorMessage(error, 'Unable to cancel Codex sign-in.'));
    for (const [name, id] of this.loginSessions) {
      if (id === sessionID) this.loginSessions.delete(name);
    }
  }

  async models(name: string) {
    const sessionID = this.loginSessions.get(name);
    if (!sessionID) throw new Error('Start Codex sign-in before loading models.');
    const { data, error } = await getSettingsPeopleCodexModels({ id: sessionID }, this.client);
    if (!data) throw new Error(errorMessage(error, 'Unable to load Codex models.'));
    return data.models.map((model) => ({ id: model.id, reasoningEfforts: model.supported_efforts }));
  }

  async check(name: string) {
    const current = this.profile(name);
    if (current?.credential_source === 'env' && !current.credential_configured) {
      throw new Error(`Set ${current.credential_env || 'the provider variable'} on daemon host and reload before checking.`);
    }
    const result = await this.api.check(name);
    const profile = this.profile(name);
    if (!profile || profile.fingerprint !== result.fingerprint) {
      throw new Error('Checked profile changed. Reload people sweep settings before granting consent.');
    }
    return { fingerprint: result.fingerprint, disclosure: {
      sourceClasses: profile.allowed_sources, since: profile.source_since, until: profile.source_until,
      sensitiveContent: profile.allow_sensitive, endpoint: profile.endpoint ?? '',
      retention: profile.retention_posture, training: profile.training_posture,
    } };
  }

  async consent(name: string, fingerprint: string, confirmed: boolean): Promise<PeopleInferenceStatus> {
    return this.capture(await this.api.consent(name, fingerprint, confirmed));
  }

  async revoke(name: string, fingerprint: string): Promise<PeopleInferenceStatus> {
    return this.capture(await this.api.revoke(name, fingerprint));
  }

  async disable(): Promise<PeopleInferenceStatus> { return this.capture(await this.api.disable()); }
  async remove(name: string): Promise<PeopleInferenceStatus> { return this.capture(await this.api.remove(name)); }

  async select(name: string, fingerprint: string): Promise<PeopleInferenceStatus> {
    const profile = this.profile(name);
    if (!profile || profile.fingerprint !== fingerprint || !profile.checked || !profile.consent_active) {
      throw new Error('Reload and check this profile before selecting it.');
    }
    return this.capture(await this.api.select(name));
  }

  private profile(name: string): PeopleInferenceProfileSetting | undefined {
    return this.source?.profiles.find((profile) => profile.name === name);
  }

  private capture(source: PeopleInferenceSettingsResponse): PeopleInferenceStatus {
    this.source = source;
    return {
      profiles: source.profiles.map((profile) => ({
        name: profile.name,
        provider: provider(profile), model: profile.model, endpoint: profile.endpoint ?? '',
        credentialStatus: credentialStatus(profile),
        credentialSource: profile.credential_source,
        credentialConfigured: profile.credential_configured,
        credentialEnv: profile.credential_env,
        fingerprint: profile.fingerprint ?? '',
        lastCheck: profile.checked ? 'Checked for this profile' : undefined,
        consentedFingerprint: profile.consent_active ? profile.fingerprint : undefined,
      })),
      configuredName: source.configured_name, runningName: source.running_name,
      configuredEnabled: source.configured_enabled, runningEnabled: source.running_enabled,
      pendingRestart: source.pending_restart,
    };
  }
}

function credentialStatus(profile: PeopleInferenceProfileSetting): string {
  if (profile.protocol === 'codex_app_server') return profile.credential_configured ? 'Signed in' : 'Sign-in needed';
  if (profile.credential_source === 'env') {
    const environment = profile.credential_env || 'the provider variable';
    return profile.credential_configured ? `Environment ${environment} ready` : `Set ${environment} on daemon host`;
  }
  return profile.credential_configured ? 'Stored key' : 'Key needed';
}

function provider(profile: PeopleInferenceProfileSetting): PeopleProvider {
  if (profile.protocol === 'codex_app_server') return 'codex';
  if (profile.preset_id === 'openai' || profile.preset_id === 'openrouter' || profile.preset_id === 'venice') {
    return profile.preset_id;
  }
  return 'custom';
}

function errorMessage(error: unknown, fallback: string): string {
  return typeof error === 'object' && error !== null && 'message' in error &&
    typeof error.message === 'string' ? error.message : fallback;
}
