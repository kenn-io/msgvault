export type PeopleProvider = 'openai' | 'openrouter' | 'venice' | 'codex' | 'custom';

export interface PeopleInferenceProfile {
  name: string;
  provider: PeopleProvider;
  model: string;
  endpoint: string;
  credentialStatus: string;
  credentialSource?: string;
  credentialConfigured?: boolean;
  credentialEnv?: string;
  fingerprint: string;
  lastCheck?: string;
  consentedFingerprint?: string;
}

export interface PeopleInferenceStatus {
  profiles: PeopleInferenceProfile[];
  configuredName?: string;
  runningName?: string;
  configuredEnabled: boolean;
  runningEnabled: boolean;
  pendingRestart: boolean;
  unavailableReason?: string;
}

export interface PeopleInferenceDisclosure {
  sourceClasses: string[];
  since: string;
  until?: string;
  sensitiveContent: boolean;
  endpoint: string;
  retention: string;
  training: string;
}

export interface PeopleInferenceCheck {
  fingerprint: string;
  disclosure: PeopleInferenceDisclosure;
}

export interface PeopleInferenceDraft {
  name: string;
  provider: PeopleProvider;
  model: string;
  reasoningEffort?: string;
  environmentVariable?: string;
  allowedSources: string[];
  sourceSince: string;
  sourceUntil?: string;
  allowSensitive: boolean;
  retentionPosture: string;
  trainingPosture: string;
}

export interface PeopleInferencePort {
  supportsCodexEnrollment?: boolean;
  supportsEnvironmentReference?: boolean;
  load(signal?: AbortSignal): Promise<PeopleInferenceStatus>;
  create(draft: PeopleInferenceDraft): Promise<PeopleInferenceStatus>;
  saveKey(name: string, key: string): Promise<PeopleInferenceStatus>;
  saveModel?(name: string, model: string, reasoningEffort: string): Promise<PeopleInferenceStatus>;
  startLogin?(name: string): Promise<{ sessionID: string; url: string; code: string; localDeadline: string }>;
  pollLogin?(sessionID: string, signal: AbortSignal): Promise<{ state: 'pending' | 'complete' | 'failed'; message?: string }>;
  cancelLogin?(sessionID: string): Promise<void>;
  models?(name: string): Promise<Array<{ id: string; reasoningEfforts: string[] }>>;
  check(name: string): Promise<PeopleInferenceCheck>;
  consent(name: string, fingerprint: string, confirmed: boolean): Promise<PeopleInferenceStatus>;
  revoke(name: string, fingerprint: string): Promise<PeopleInferenceStatus>;
  disable(): Promise<PeopleInferenceStatus>;
  remove(name: string): Promise<PeopleInferenceStatus>;
  select(name: string, fingerprint: string): Promise<PeopleInferenceStatus>;
}

export class PeopleInferenceController {
  status = $state<PeopleInferenceStatus>();
  selectedName = $state('');
  loading = $state(true);
  busy = $state(false);
  error = $state('');
  login = $state<{ sessionID: string; url: string; code: string; localDeadline: string }>();
  loginState = $state<'idle' | 'pending' | 'complete' | 'failed'>('idle');
  availableModels = $state<Array<{ id: string; reasoningEfforts: string[] }>>([]);
  selectedModel = $state('');
  reasoningEffort = $state('');
  checkResult = $state<PeopleInferenceCheck>();
  disclosureConfirmed = $state(false);
  consentedFingerprint = $state('');

  private destroyed = false;
  private selectionEpoch = 0;
  private pollAbort?: AbortController;
  private pollTimer?: ReturnType<typeof setTimeout>;

  constructor(private readonly port: PeopleInferencePort) {}

  get supportsCodex(): boolean {
    return Boolean(this.port.supportsCodexEnrollment && this.port.startLogin && this.port.pollLogin &&
      this.port.cancelLogin && this.port.models);
  }

  get selectedProfile(): PeopleInferenceProfile | undefined {
    return this.status?.profiles.find((profile) => profile.name === this.selectedName);
  }

  get canSelect(): boolean {
    const fingerprint = this.selectedProfile?.fingerprint;
    return Boolean(fingerprint && this.checkResult?.fingerprint === fingerprint &&
      this.selectedProfile?.consentedFingerprint === fingerprint && this.consentedFingerprint === fingerprint);
  }

  get canConsent(): boolean {
    return Boolean(this.disclosureConfirmed && this.checkResult?.fingerprint &&
      this.checkResult.fingerprint === this.selectedProfile?.fingerprint);
  }

  async load(): Promise<void> {
    const selectionEpoch = this.selectionEpoch;
    this.loading = true;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    try {
      const status = await this.port.load();
      if (this.destroyed) return;
      this.status = status;
      if (this.selectionEpoch === selectionEpoch) {
        this.selectedName = status.configuredName ?? status.profiles[0]?.name ?? '';
      }
      this.error = '';
    } catch (cause) {
      if (!this.destroyed) {
        this.status = undefined;
        this.error = message(cause, 'Unable to load people sweep settings.');
      }
    } finally {
      if (!this.destroyed) this.loading = false;
    }
  }

  choose(name: string): void {
    this.selectionEpoch += 1;
    if (this.login) {
      const sessionID = this.login.sessionID;
      this.stopPolling();
      void this.port.cancelLogin?.(sessionID).catch((cause) => {
        if (!this.destroyed) this.error = message(cause, 'Unable to cancel Codex sign-in.');
      });
    }
    this.selectedName = name;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    this.availableModels = [];
    this.selectedModel = '';
    this.reasoningEffort = '';
    this.login = undefined;
    this.loginState = 'idle';
  }

  async create(draft: PeopleInferenceDraft, key: string): Promise<void> {
    await this.run(async () => {
      let status = await this.port.create(draft);
      if (key && draft.provider !== 'codex') status = await this.port.saveKey(draft.name, key);
      if (this.destroyed) return;
      this.status = status;
      if (draft.provider === 'codex') this.login = undefined; // Profile creation consumes the server draft.
      this.choose(draft.name);
    }, 'Unable to create profile.');
  }

  async saveKey(key: string): Promise<void> {
    const name = this.selectedName;
    if (!name || !key || this.selectedProfile?.provider === 'codex') return;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    await this.run(async () => {
      this.status = await this.port.saveKey(name, key);
    }, 'Unable to save people provider key.');
  }

  async startLogin(name = this.selectedName): Promise<void> {
    if (!name || this.busy || !this.port.startLogin || !this.supportsCodex) return;
    const selectionEpoch = this.selectionEpoch;
    const existingProfile = this.status?.profiles.some((profile) => profile.name === name && profile.provider === 'codex') ?? false;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    this.availableModels = [];
    this.selectedModel = '';
    this.reasoningEffort = '';
    await this.run(async () => {
      const login = await this.port.startLogin!(name);
      if (this.destroyed || this.selectionEpoch !== selectionEpoch) {
        await this.port.cancelLogin?.(login.sessionID);
        return;
      }
      this.login = login;
      this.loginState = 'pending';
      void this.pollLogin(name, login.sessionID, existingProfile);
    }, 'Unable to start Codex sign-in.');
  }

  private async pollLogin(name: string, sessionID: string, existingProfile: boolean): Promise<void> {
    if (!this.port.pollLogin) return;
    const pollAbort = new AbortController();
    this.pollAbort = pollAbort;
    try {
      const result = await this.port.pollLogin(sessionID, pollAbort.signal);
      if (this.destroyed || this.login?.sessionID !== sessionID) return;
      if (result.state === 'pending') {
        this.pollTimer = setTimeout(() => void this.pollLogin(name, sessionID, existingProfile), 2000);
      } else if (result.state === 'complete') {
        if (existingProfile) {
          const status = await this.port.load();
          if (this.destroyed || this.login?.sessionID !== sessionID) return;
          this.status = status;
          const profile = status.profiles.find((item) => item.name === name);
          if (!profile || profile.credentialConfigured === false) {
            this.failLogin(sessionID, 'Codex sign-in completed, but the daemon has no usable auth. Reload and try again.');
            return;
          }
          if (profile.model) {
            this.login = undefined;
            this.loginState = 'idle';
            void this.port.cancelLogin?.(sessionID).catch((cause) => {
              if (!this.destroyed) this.error = message(cause, 'Unable to close Codex sign-in.');
            });
            return;
          }
        }
        this.loginState = 'complete';
        const models = await this.port.models?.(name) ?? [];
        if (this.destroyed || this.login?.sessionID !== sessionID) return;
        this.availableModels = models;
        this.selectedModel = this.availableModels[0]?.id ?? '';
        this.reasoningEffort = this.availableModels[0]?.reasoningEfforts[0] ?? '';
      } else {
        this.failLogin(sessionID, result.message ?? 'Codex sign-in failed.');
      }
    } catch (cause) {
      if (!this.destroyed && this.login?.sessionID === sessionID && !pollAbort.signal.aborted) {
        this.failLogin(sessionID, message(cause, 'Unable to check Codex sign-in.'));
      }
    }
  }

  private failLogin(sessionID: string, reason: string): void {
    this.stopPolling();
    this.login = undefined;
    this.loginState = 'failed';
    this.error = reason;
    void this.port.cancelLogin?.(sessionID).catch((cause) => {
      if (!this.destroyed && this.error === reason) {
        this.error = `${reason} ${message(cause, 'Unable to close Codex sign-in.')}`;
      }
    });
  }

  async cancelLogin(): Promise<void> {
    this.stopPolling();
    const sessionID = this.login?.sessionID;
    this.loginState = 'idle';
    this.login = undefined;
    this.availableModels = [];
    this.selectedModel = '';
    this.reasoningEffort = '';
    if (sessionID) await this.port.cancelLogin?.(sessionID);
  }

  async check(): Promise<void> {
    const name = this.selectedName;
    const selectionEpoch = this.selectionEpoch;
    if (!name) return;
    if (this.selectedProfile?.credentialSource === 'env' && !this.selectedProfile.credentialConfigured) {
      this.error = `Set ${this.selectedProfile.credentialEnv || 'the provider variable'} on daemon host and reload before checking.`;
      return;
    }
    if (this.selectedProfile?.provider === 'codex' && this.selectedProfile.credentialConfigured === false) {
      this.error = 'Sign in with Codex and reload daemon status before checking.';
      return;
    }
    // A repeat check replaces the prior result. A failure cannot leave the
    // old check and consent actionable for a profile that may have changed.
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    await this.run(async () => {
      if (this.selectedProfile?.provider === 'codex' && this.selectedModel && this.port.saveModel) {
        const status = await this.port.saveModel(name, this.selectedModel, this.reasoningEffort);
        if (this.destroyed || this.selectedName !== name || this.selectionEpoch !== selectionEpoch) return;
        this.status = status;
      }
      const result = await this.port.check(name);
      if (this.destroyed || this.selectedName !== name || this.selectionEpoch !== selectionEpoch) return;
      // Check and status are separate server reads. Compare the returned
      // fingerprint with a fresh profile before allowing consent or selection.
      const status = await this.port.load();
      if (this.destroyed || this.selectedName !== name || this.selectionEpoch !== selectionEpoch) return;
      this.status = status;
      this.checkResult = result;
      this.consentedFingerprint = '';
      this.disclosureConfirmed = false;
    }, 'Synthetic check failed.');
  }

  async consent(): Promise<void> {
    const fingerprint = this.checkResult?.fingerprint;
    const name = this.selectedName;
    if (!name || !fingerprint || !this.canConsent) return;
    await this.run(async () => {
      this.status = await this.port.consent(name, fingerprint, this.disclosureConfirmed);
      if (this.status.profiles.find((profile) => profile.name === name)?.consentedFingerprint !== fingerprint) {
        throw new Error('Consent was not recorded for the checked profile. Reload and try again.');
      }
      this.consentedFingerprint = fingerprint;
    }, 'Unable to grant consent.');
  }

  async select(): Promise<void> {
    if (!this.canSelect || !this.checkResult) return;
    await this.run(async () => {
      this.status = await this.port.select(this.selectedName, this.checkResult!.fingerprint);
    }, 'Unable to select profile.');
  }

  async revoke(): Promise<void> {
    const name = this.selectedName;
    const fingerprint = this.selectedProfile?.fingerprint;
    if (!name || !fingerprint || this.selectedProfile?.consentedFingerprint !== fingerprint) return;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    await this.run(async () => {
      this.status = await this.port.revoke(name, fingerprint);
      if (this.status.profiles.find((profile) => profile.name === name)?.consentedFingerprint === fingerprint) {
        throw new Error('Consent revocation was not reflected in provider status. Reload and try again.');
      }
    }, 'Unable to revoke consent.');
  }

  async disable(): Promise<void> {
    if (!this.status?.configuredEnabled) return;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    await this.run(async () => {
      if (this.loginState === 'pending') await this.cancelLogin();
      this.status = await this.port.disable();
      if (this.status.configuredEnabled) {
        throw new Error('People sweep is still enabled in provider status. Reload and try again.');
      }
    }, 'Unable to disable people sweep.');
  }

  async remove(name: string): Promise<void> {
    if (name !== this.selectedName || !this.status?.profiles.some((profile) => profile.name === name) ||
      this.status.profiles.length < 2 || (this.status.configuredEnabled && this.status.configuredName === name)) return;
    this.checkResult = undefined;
    this.consentedFingerprint = '';
    this.disclosureConfirmed = false;
    await this.run(async () => {
      if (this.loginState === 'pending') await this.cancelLogin();
      const status = await this.port.remove(name);
      if (this.destroyed) return;
      if (status.profiles.some((profile) => profile.name === name)) {
        throw new Error('Removed profile remains in provider status. Reload and try again.');
      }
      this.status = status;
      this.choose(status.profiles.find((profile) => profile.name === status.configuredName)?.name ??
        status.profiles[0]?.name ?? '');
    }, 'Unable to remove profile.');
  }

  destroy(): void {
    this.destroyed = true;
    this.stopPolling();
    if (this.login) void this.port.cancelLogin?.(this.login.sessionID).catch(() => undefined);
  }

  private stopPolling(): void {
    this.pollAbort?.abort();
    if (this.pollTimer) clearTimeout(this.pollTimer);
  }

  private async run(action: () => Promise<void>, fallback: string): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    this.error = '';
    try {
      await action();
    } catch (cause) {
      if (!this.destroyed) this.error = message(cause, fallback);
    } finally {
      if (!this.destroyed) this.busy = false;
    }
  }
}

function message(cause: unknown, fallback: string): string {
  return cause instanceof Error && cause.message ? cause.message : fallback;
}
