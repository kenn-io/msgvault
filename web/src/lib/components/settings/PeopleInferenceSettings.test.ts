import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import PeopleInferenceSettings from './PeopleInferenceSettings.svelte';
import { PeopleInferenceController, type PeopleInferencePort,
  type PeopleInferenceStatus } from '../../settings/people-inference-controller.svelte';

const disclosure = {
  sourceClasses: ['messages', 'calendar'],
  since: '2025-01-01',
  until: '2025-12-31',
  sensitiveContent: false,
  endpoint: 'https://openrouter.ai/api/v1',
  retention: 'Operator assertion: no retention',
  training: 'Operator assertion: no training',
};

function fixture(overrides: Partial<PeopleInferencePort> = {}): PeopleInferencePort {
  let status: PeopleInferenceStatus = {
    profiles: [], configuredEnabled: false, runningEnabled: false, pendingRestart: false,
  };
  const port: PeopleInferencePort = {
    supportsCodexEnrollment: true,
    load: vi.fn(async () => status),
    create: vi.fn(async (draft) => {
      status = { ...status, profiles: [...status.profiles, {
        name: draft.name, provider: draft.provider, model: draft.model,
        endpoint: draft.provider === 'openrouter' ? disclosure.endpoint : 'https://api.venice.ai/api/v1',
        credentialStatus: draft.provider === 'codex' ? 'Signed out' : 'Key needed',
        fingerprint: `fingerprint-${draft.name}`,
      }] };
      return status;
    }),
    saveKey: vi.fn(async () => status),
    startLogin: vi.fn(async () => ({
      sessionID: 'session-1', url: 'https://example.test/device', code: 'ABCD-EFGH',
      localDeadline: '2026-09-23T07:00:00Z',
    })),
    pollLogin: vi.fn(async () => ({ state: 'complete' as const })),
    cancelLogin: vi.fn(async () => undefined),
    models: vi.fn(async () => [{ id: 'codex-model', reasoningEfforts: ['low', 'medium'] }]),
    check: vi.fn(async (name) => ({ fingerprint: `fingerprint-${name}`, disclosure })),
    consent: vi.fn(async (name, fingerprint) => {
      status = { ...status, profiles: status.profiles.map((profile) => profile.name === name
        ? { ...profile, consentedFingerprint: fingerprint } : profile) };
      return status;
    }),
    revoke: vi.fn(async (name) => {
      status = { ...status, profiles: status.profiles.map((profile) => profile.name === name
        ? { ...profile, consentedFingerprint: undefined } : profile) };
      return status;
    }),
    disable: vi.fn(async () => {
      status = { ...status, configuredEnabled: false, pendingRestart: true };
      return status;
    }),
    remove: vi.fn(async (name) => {
      status = { ...status, profiles: status.profiles.filter((profile) => profile.name !== name) };
      return status;
    }),
    select: vi.fn(async (name) => {
      status = { ...status, configuredName: name, configuredEnabled: true, pendingRestart: true };
      return status;
    }),
    ...overrides,
  };
  return port;
}

afterEach(() => vi.useRealTimers());

async function fillArchivePolicy(): Promise<void> {
  await fireEvent.click(screen.getByLabelText('Conversation text'));
  await fireEvent.input(screen.getByLabelText(/^Archive data since/), { target: { value: '2025-01-01' } });
  await fireEvent.input(screen.getByLabelText('Retention statement'), { target: { value: 'Operator assertion: no retention' } });
  await fireEvent.input(screen.getByLabelText('Training statement'), { target: { value: 'Operator assertion: no training' } });
  await fireEvent.click(screen.getByLabelText('Allow sensitive content'));
}

describe('PeopleInferenceSettings', () => {
  it('reauthenticates a saved Codex profile with a model, then requires a fresh check and consent', async () => {
    const profile = {
      name: 'subscription', provider: 'codex' as const, model: 'codex-model', endpoint: '',
      credentialStatus: 'Sign-in needed', credentialSource: 'none', credentialConfigured: false,
      fingerprint: 'codex-fingerprint', consentedFingerprint: 'codex-fingerprint',
    };
    let current: PeopleInferenceStatus = { profiles: [profile], configuredEnabled: false,
      runningEnabled: false, pendingRestart: false };
    const port = fixture({
      load: vi.fn(async () => current),
      check: vi.fn(async () => ({ fingerprint: 'codex-fingerprint', disclosure })),
      startLogin: vi.fn(async () => {
        current = { ...current, profiles: [{ ...profile, credentialStatus: 'Signed in',
          credentialConfigured: true, consentedFingerprint: undefined }] };
        return { sessionID: 'session-reauth', url: 'https://example.test/device', code: 'ABCD-EFGH',
          localDeadline: '2026-09-23T07:00:00Z' };
      }),
      consent: vi.fn(async (_name, fingerprint) => {
        current = { ...current, profiles: current.profiles.map((item) => ({ ...item,
          consentedFingerprint: fingerprint })) };
        return current;
      }),
    });
    render(PeopleInferenceSettings, { port });
    expect(await screen.findByText('Sign-in needed')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Check provider' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    expect(await screen.findByText('Signed in')).toBeDefined();
    expect(screen.getByText('Not granted')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Select and enable' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    expect(await screen.findByText(/Retention: Operator assertion/)).toBeDefined();
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Select and enable' }) as HTMLButtonElement).disabled).toBe(false));
    expect(port.check).toHaveBeenCalledWith('subscription');
  });

  it('can retry when completed Codex reauthentication has no usable daemon auth', async () => {
    const port = fixture({
      load: vi.fn(async () => ({ profiles: [{
        name: 'subscription', provider: 'codex' as const, model: 'codex-model', endpoint: '',
        credentialStatus: 'Sign-in needed', credentialConfigured: false,
        fingerprint: 'codex-fingerprint',
      }], configuredEnabled: false, runningEnabled: false, pendingRestart: false })),
    });
    render(PeopleInferenceSettings, { port });
    await screen.findByText('Sign-in needed');
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Reload and try again.');
    await waitFor(() => {
      expect((screen.getByRole('button', { name: 'Reload people sweep settings' }) as HTMLButtonElement).disabled).toBe(false);
      expect((screen.getByRole('button', { name: 'Sign in with Codex' }) as HTMLButtonElement).disabled).toBe(false);
    });
    expect(port.cancelLogin).toHaveBeenCalledWith('session-1');
    await fireEvent.click(screen.getByRole('button', { name: 'Reload people sweep settings' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await waitFor(() => expect(port.startLogin).toHaveBeenCalledTimes(2));
  });

  it('locks Codex draft identity while StartLogin is awaiting the device code', async () => {
    let finishStart!: (login: { sessionID: string; url: string; code: string; localDeadline: string }) => void;
    const start = new Promise<{ sessionID: string; url: string; code: string; localDeadline: string }>((resolve) => {
      finishStart = resolve;
    });
    const port = fixture({ startLogin: vi.fn(async () => start) });
    const rendered = render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'codex' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'subscription' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    expect((screen.getByLabelText('Provider') as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByLabelText('Profile name') as HTMLInputElement).disabled).toBe(true);
    rendered.unmount();
    finishStart({ sessionID: 'late-session', url: 'https://example.test/device', code: 'ABCD-EFGH',
      localDeadline: '2026-09-23T07:00:00Z' });
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('late-session'));
  });

  it('cancels a delayed StartLogin response after profile selection changes', async () => {
    let finishStart!: (login: { sessionID: string; url: string; code: string; localDeadline: string }) => void;
    const start = new Promise<{ sessionID: string; url: string; code: string; localDeadline: string }>((resolve) => {
      finishStart = resolve;
    });
    const port = fixture({
      load: vi.fn(async () => ({ profiles: [
        { name: 'first', provider: 'codex' as const, model: 'codex-model', endpoint: '',
          credentialStatus: 'Sign-in needed', fingerprint: 'first' },
        { name: 'second', provider: 'codex' as const, model: 'codex-model', endpoint: '',
          credentialStatus: 'Sign-in needed', fingerprint: 'second' },
      ], configuredEnabled: false, runningEnabled: false, pendingRestart: false })),
      startLogin: vi.fn(async () => start),
    });
    const controller = new PeopleInferenceController(port);
    await controller.load();
    const starting = controller.startLogin();
    controller.choose('second');
    finishStart({ sessionID: 'late-session', url: 'https://example.test/device', code: 'ABCD-EFGH',
      localDeadline: '2026-09-23T07:00:00Z' });
    await starting;
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('late-session'));
    expect(controller.login).toBeUndefined();
    controller.destroy();
  });

  it('keeps a new Codex login when an aborted prior poll rejects late', async () => {
    let rejectFirst!: (cause: Error) => void;
    const firstPoll = new Promise<{ state: 'pending' }>((_resolve, reject) => { rejectFirst = reject; });
    let starts = 0;
    const port = fixture({
      load: vi.fn(async () => ({ profiles: ['first', 'second'].map((name) => ({
        name, provider: 'codex' as const, model: '', endpoint: '', credentialStatus: 'Sign-in needed',
        fingerprint: name,
      })), configuredEnabled: false, runningEnabled: false, pendingRestart: false })),
      startLogin: vi.fn(async () => ({ sessionID: `session-${++starts}`,
        url: 'https://example.test/device', code: 'ABCD-EFGH', localDeadline: '2026-09-23T07:00:00Z' })),
      pollLogin: vi.fn(async (sessionID) => sessionID === 'session-1'
        ? firstPoll : { state: 'pending' as const }),
    });
    const controller = new PeopleInferenceController(port);
    await controller.load();
    await controller.startLogin();
    controller.choose('second');
    await controller.startLogin();
    rejectFirst(new Error('Old poll failed late'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(controller.login?.sessionID).toBe('session-2');
    expect(controller.loginState).toBe('pending');
    controller.destroy();
  });

  it('does not write a stale HTTP key when the draft switches to Codex', async () => {
    const port = fixture();
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-old-key' } });
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'codex' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'subscription' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await screen.findByText('Signed in. Choose a model and reasoning effort.');
    await fillArchivePolicy();
    await fireEvent.click(screen.getByRole('button', { name: 'Create Codex profile' }));
    await waitFor(() => expect(port.create).toHaveBeenCalledOnce());
    expect(port.saveKey).not.toHaveBeenCalled();
  });

  it('drops models from a login session after switching profiles while model discovery waits', async () => {
    let finishModels!: (models: Array<{ id: string; reasoningEfforts: string[] }>) => void;
    const models = new Promise<Array<{ id: string; reasoningEfforts: string[] }>>((resolve) => { finishModels = resolve; });
    const profiles = ['first', 'second'].map((name) => ({
      name, provider: 'codex' as const, model: '', endpoint: '', credentialStatus: 'Signed out', fingerprint: name,
    }));
    const port = fixture({
      load: vi.fn(async () => ({ profiles, configuredEnabled: false, runningEnabled: false, pendingRestart: false })),
      models: vi.fn(async () => models),
    });
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Profile');
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await waitFor(() => expect(port.models).toHaveBeenCalledWith('first'));
    await fireEvent.change(screen.getByLabelText('Profile'), { target: { value: 'second' } });
    finishModels([{ id: 'stale-model', reasoningEfforts: ['high'] }]);
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('session-1'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText('stale-model')).toBeNull();
  });

  it('drops a checked disclosure when profile selection changes during the server refresh', async () => {
    let finishLoad!: (status: PeopleInferenceStatus) => void;
    const refresh = new Promise<PeopleInferenceStatus>((resolve) => { finishLoad = resolve; });
    const profiles = ['first', 'second'].map((name) => ({
      name, provider: 'openrouter' as const, model: 'model-one', endpoint: disclosure.endpoint,
      credentialStatus: 'Stored key', fingerprint: name,
    }));
    const status = { profiles, configuredEnabled: false, runningEnabled: false, pendingRestart: false };
    let reads = 0;
    const port = fixture({ load: vi.fn(async () => (++reads === 1 ? status : refresh)),
      check: vi.fn(async () => ({ fingerprint: 'first', disclosure })) });
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Profile');
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await waitFor(() => expect(port.load).toHaveBeenCalledTimes(2));
    await fireEvent.change(screen.getByLabelText('Profile'), { target: { value: 'second' } });
    finishLoad(status);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check provider' }).hasAttribute('disabled')).toBe(false));
    expect(screen.queryByText(/Retention: Operator assertion/)).toBeNull();
  });
  it('replaces a saved HTTP key without leaving the old check selectable or rendering the key', async () => {
    const initial: PeopleInferenceStatus = {
      profiles: [{ name: 'routed', provider: 'openrouter', model: 'model-one', endpoint: disclosure.endpoint,
        credentialStatus: 'Stored key', fingerprint: 'before-key' }],
      configuredEnabled: false, runningEnabled: false, pendingRestart: false,
    };
    const port = fixture({
      load: vi.fn(async () => initial),
      saveKey: vi.fn(async () => ({ ...initial, profiles: [{ ...initial.profiles[0]!,
        fingerprint: 'after-key' }] })),
    });
    render(PeopleInferenceSettings, { port });
    await screen.findByText('Stored key');
    await fireEvent.input(screen.getByLabelText('Replacement API key'), { target: { value: 'synthetic-replacement' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save API key' }));
    await waitFor(() => expect(port.saveKey).toHaveBeenCalledWith('routed', 'synthetic-replacement'));
    await waitFor(() => expect((screen.getByLabelText('Replacement API key') as HTMLInputElement).value).toBe(''));
    expect(document.body.textContent).not.toContain('synthetic-replacement');
    expect((screen.getByRole('button', { name: 'Select and enable' }) as HTMLButtonElement).disabled).toBe(true);
  }, 15000);

  it('requires an explicit archive policy and sensitive-content choice for an HTTP profile', async () => {
    const port = fixture();
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'openrouter' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'routed' } });
    await fireEvent.input(screen.getByLabelText('Model ID'), { target: { value: 'model-one' } });
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-secret' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
    expect(port.create).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByLabelText('Conversation text'));
    await fireEvent.input(screen.getByLabelText(/^Archive data since/), { target: { value: '2025-01-01' } });
    await fireEvent.input(screen.getByLabelText('Retention statement'), { target: { value: 'Operator assertion: no retention' } });
    await fireEvent.input(screen.getByLabelText('Training statement'), { target: { value: 'Operator assertion: no training' } });
    await fireEvent.click(screen.getByLabelText('Allow sensitive content'));
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));

    expect(port.create).toHaveBeenCalledWith({
      name: 'routed', provider: 'openrouter', model: 'model-one', environmentVariable: undefined,
      allowedSources: ['conversation_text'], sourceSince: '2025-01-01', sourceUntil: undefined,
      allowSensitive: true, retentionPosture: 'Operator assertion: no retention',
      trainingPosture: 'Operator assertion: no training',
    });
  });

  it('completes device login without granting consent, then gates selection on check and exact disclosure', async () => {
    const port = fixture();
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'codex' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'subscription' } });
    await fillArchivePolicy();
    expect((screen.getByRole('button', { name: 'Create Codex profile' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    expect(await screen.findByText('ABCD-EFGH')).toBeDefined();
    expect(screen.getByText('https://example.test/device')).toBeDefined();
    expect(screen.getByText(/Local sign-in session ends/)).toBeDefined();
    await waitFor(() => expect(port.models).toHaveBeenCalledWith('subscription'));
    expect(screen.getByLabelText('Model')).toBeDefined();
    expect(screen.getByLabelText('Reasoning effort')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Create Codex profile' }));
    await waitFor(() => expect(port.create).toHaveBeenCalledWith(expect.objectContaining({
      name: 'subscription', provider: 'codex', model: 'codex-model', reasoningEffort: 'low',
    })));
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);

    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    expect(await screen.findByText(/Retention: Operator assertion: no retention/)).toBeDefined();
    expect(screen.getByText(/Source classes: messages, calendar/)).toBeDefined();
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Select and enable' }));
    expect(await screen.findByText(/Restart the daemon/)).toBeDefined();
    expect(port.select).toHaveBeenCalledWith('subscription', 'fingerprint-subscription');
  }, 15000);

  it('cancels an unfinished Codex draft when leaving Settings after sign-in', async () => {
    const port = fixture();
    const rendered = render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'codex' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'subscription' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await screen.findByText('ABCD-EFGH');
    await waitFor(() => expect(port.models).toHaveBeenCalledWith('subscription'));
    rendered.unmount();
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('session-1'));
    expect(port.create).not.toHaveBeenCalled();
  });

  it.each(['openrouter', 'venice'] as const)('keeps a %s key masked and requires check and consent', async (provider) => {
    const port = fixture();
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: provider } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: `${provider}-profile` } });
    await fireEvent.input(screen.getByLabelText('Model ID'), { target: { value: 'model-one' } });
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-secret' } });
    expect((screen.getByLabelText('API key') as HTMLInputElement).type).toBe('password');
    await fillArchivePolicy();
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
    await waitFor(() => expect(port.saveKey).toHaveBeenCalledWith(`${provider}-profile`, 'synthetic-secret'));
    expect(screen.queryByText('synthetic-secret')).toBeNull();
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(false);
  });

  it('uses the key-write status and blocks selection if the refreshed profile differs from the checked fingerprint', async () => {
    const profile = (fingerprint: string, credentialStatus: string) => ({
      name: 'routed', provider: 'openrouter' as const, model: 'model-one',
      endpoint: disclosure.endpoint, credentialStatus, fingerprint,
    });
    const status = (fingerprint: string, credentialStatus: string): PeopleInferenceStatus => ({
      profiles: [profile(fingerprint, credentialStatus)], configuredEnabled: false,
      runningEnabled: false, pendingRestart: false,
    });
    let reads = 0;
    const port = fixture({
      load: vi.fn(async () => {
        reads += 1;
        return reads === 1 ? status('before-key', 'Key needed') : status('changed-after-check', 'Stored key');
      }),
      create: vi.fn(async () => status('before-key', 'Key needed')),
      saveKey: vi.fn(async () => status('after-key', 'Stored key')),
      check: vi.fn(async () => ({ fingerprint: 'after-key', disclosure })),
    });
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'openrouter' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'routed' } });
    await fireEvent.input(screen.getByLabelText('Model ID'), { target: { value: 'model-one' } });
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-secret' } });
    await fillArchivePolicy();
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
    expect(await screen.findByText('Stored key')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await screen.findByText(/Retention: Operator assertion/);
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    expect(port.load).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    expect(port.select).not.toHaveBeenCalled();
  });

  it.each([undefined, 'fingerprint-other'])('requires the consent response to record the checked fingerprint (%s)', async (consentedFingerprint) => {
    const status: PeopleInferenceStatus = {
      profiles: [{ name: 'routed', provider: 'openrouter', model: 'model-one',
        endpoint: disclosure.endpoint, credentialStatus: 'Stored key',
        fingerprint: 'fingerprint-routed', consentedFingerprint }],
      configuredEnabled: false, runningEnabled: false, pendingRestart: false,
    };
    const port = fixture({
      load: vi.fn(async () => status),
      consent: vi.fn(async () => status),
    });
    render(PeopleInferenceSettings, { port });

    await fireEvent.click(await screen.findByRole('button', { name: 'Check provider' }));
    await screen.findByRole('region', { name: 'Archive disclosure' });
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));

    expect(port.consent).toHaveBeenCalledWith('routed', 'fingerprint-routed', true);
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    expect(port.select).not.toHaveBeenCalled();
    expect(screen.getByText('Not granted')).toBeDefined();
  });

  it('revokes the local selection gate when a repeat synthetic check fails', async () => {
    const port = fixture({
      check: vi.fn()
        .mockResolvedValueOnce({ fingerprint: 'fingerprint-routed', disclosure })
        .mockRejectedValueOnce(new Error('Provider unavailable')),
    });
    render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Provider');
    await fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'openrouter' } });
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'routed' } });
    await fireEvent.input(screen.getByLabelText('Model ID'), { target: { value: 'model-one' } });
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-secret' } });
    await fillArchivePolicy();
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
    await screen.findByRole('button', { name: 'Check provider' });
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await screen.findByText(/Retention: Operator assertion/);
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Provider unavailable');
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
  });

  it('cancels a pending device session when switching profiles or leaving the view', async () => {
    const codexProfile = (name: string) => ({
      name, provider: 'codex' as const, model: '', endpoint: '', credentialStatus: 'Signed out',
      fingerprint: `fingerprint-${name}`,
    });
    const signals: AbortSignal[] = [];
    const port = fixture({
      load: vi.fn(async () => ({
        profiles: [codexProfile('first'), codexProfile('second')],
        configuredEnabled: false, runningEnabled: false, pendingRestart: false,
      })),
      pollLogin: vi.fn(async (_sessionID, signal: AbortSignal) => {
        signals.push(signal);
        return { state: 'pending' as const };
      }),
    });
    const rendered = render(PeopleInferenceSettings, { port });
    await screen.findByLabelText('Profile');
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await screen.findByText('ABCD-EFGH');
    await fireEvent.change(screen.getByLabelText('Profile'), { target: { value: 'second' } });
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('session-1'));
    expect(signals[0]?.aborted).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Sign in with Codex' }));
    await screen.findByText('ABCD-EFGH');
    rendered.unmount();
    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledTimes(2));
  });

  it('reports an unsupported Codex host without showing a device code or granting consent', async () => {
    const port = fixture({
      load: vi.fn(async () => ({
        profiles: [{ name: 'subscription', provider: 'codex' as const, model: '', endpoint: '',
          credentialStatus: 'Signed out', fingerprint: 'fingerprint-subscription' }],
        configuredEnabled: false, runningEnabled: false, pendingRestart: false,
      })),
      startLogin: vi.fn(async () => { throw new Error('Codex device login is unavailable on this host.'); }),
    });
    render(PeopleInferenceSettings, { port });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sign in with Codex' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Codex device login is unavailable on this host.');
    expect(screen.queryByText('ABCD-EFGH')).toBeNull();
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    expect(port.consent).not.toHaveBeenCalled();
  });

  it('shows configured versus running restart state without rendering a loaded secret', async () => {
    const profile = {
      name: 'routed', provider: 'openrouter' as const, model: 'model-one',
      endpoint: disclosure.endpoint, credentialStatus: 'Stored key',
      fingerprint: 'fingerprint-routed', secret: 'synthetic-secret-must-not-render',
    };
    const port = fixture({
      load: vi.fn(async () => ({
        profiles: [profile], configuredName: 'routed', runningName: 'old-profile',
        configuredEnabled: true, runningEnabled: true, pendingRestart: true,
      })),
    });
    const rendered = render(PeopleInferenceSettings, { port });

    expect((await screen.findByText(/Restart the daemon/)).textContent).toContain('old-profile');
    expect(screen.getByText('Stored key')).toBeDefined();
    expect(rendered.container.textContent).not.toContain('synthetic-secret-must-not-render');
    expect(screen.queryByDisplayValue('synthetic-secret-must-not-render')).toBeNull();
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
  });

  it('revokes exact profile consent and disables configured sweeps while showing the running state', async () => {
    let status: PeopleInferenceStatus = {
      profiles: [{ name: 'routed', provider: 'openrouter', model: 'model-one',
        endpoint: disclosure.endpoint, credentialStatus: 'Stored key',
        fingerprint: 'fingerprint-routed', consentedFingerprint: 'fingerprint-routed' }],
      configuredName: 'routed', runningName: 'routed',
      configuredEnabled: true, runningEnabled: true, pendingRestart: false,
    };
    const revoke = vi.fn(async () => {
      status = { ...status, profiles: status.profiles.map((profile) => ({
        ...profile, consentedFingerprint: undefined,
      })) };
      return status;
    });
    const disable = vi.fn(async () => {
      status = { ...status, configuredEnabled: false, pendingRestart: true };
      return status;
    });
    const port = Object.assign(fixture({ load: vi.fn(async () => status) }), { revoke, disable });
    render(PeopleInferenceSettings, { port });

    await fireEvent.click(await screen.findByRole('button', { name: 'Revoke consent' }));
    expect(revoke).toHaveBeenCalledWith('routed', 'fingerprint-routed');
    expect(screen.getByText('Not granted')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Select and enable' }).hasAttribute('disabled')).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Disable people sweep' }));
    expect(disable).toHaveBeenCalledTimes(1);
    expect((await screen.findByText(/Restart the daemon/)).textContent).toContain('routed');
    expect(screen.getByText('Configured:').parentElement?.textContent).toContain('disabled');
  });

  it('cancels a pending Codex device session before disabling people sweep', async () => {
    const status: PeopleInferenceStatus = {
      profiles: [{ name: 'subscription', provider: 'codex', model: '', endpoint: '',
        credentialStatus: 'Signed out', fingerprint: 'fingerprint-subscription' }],
      configuredName: 'subscription', runningName: 'subscription',
      configuredEnabled: true, runningEnabled: true, pendingRestart: false,
    };
    const signals: AbortSignal[] = [];
    const port = fixture({
      load: vi.fn(async () => status),
      pollLogin: vi.fn(async (_sessionID, signal) => {
        signals.push(signal);
        return { state: 'pending' as const };
      }),
      disable: vi.fn(async () => ({ ...status, configuredEnabled: false, pendingRestart: true })),
    });
    render(PeopleInferenceSettings, { port });

    await fireEvent.click(await screen.findByRole('button', { name: 'Sign in with Codex' }));
    await screen.findByText('ABCD-EFGH');
    await fireEvent.click(screen.getByRole('button', { name: 'Disable people sweep' }));

    await waitFor(() => expect(port.cancelLogin).toHaveBeenCalledWith('session-1'));
    expect(signals[0]?.aborted).toBe(true);
    expect(port.disable).toHaveBeenCalledTimes(1);
    expect(screen.queryByText('ABCD-EFGH')).toBeNull();
  });

  it('requires confirmation before removing an inactive profile and selects a remaining profile', async () => {
    const profile = (name: string) => ({ name, provider: 'openrouter' as const,
      model: 'model-one', endpoint: disclosure.endpoint, credentialStatus: 'Stored key',
      fingerprint: `fingerprint-${name}` });
    let status: PeopleInferenceStatus = {
      profiles: [profile('routed'), profile('spare')],
      configuredName: 'spare', runningName: 'spare',
      configuredEnabled: true, runningEnabled: true, pendingRestart: false,
    };
    const remove = vi.fn(async (name: string) => {
      status = { ...status, profiles: status.profiles.filter((item) => item.name !== name),
        pendingRestart: true };
      return status;
    });
    const port = Object.assign(fixture({ load: vi.fn(async () => status) }), { remove });
    render(PeopleInferenceSettings, { port });

    const activeRemove = await screen.findByRole('button', { name: 'Remove profile' });
    expect(activeRemove.hasAttribute('disabled')).toBe(true);
    await fireEvent.change(screen.getByLabelText('Profile'), { target: { value: 'routed' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Remove profile' }));
    expect(screen.getByText(/removes its stored credential/)).toBeDefined();
    expect(remove).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel removal' }));
    expect(remove).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', { name: 'Remove profile' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));

    expect(remove).toHaveBeenCalledWith('routed');
    expect((screen.getByLabelText('Profile') as HTMLSelectElement).value).toBe('spare');
    expect(screen.queryByRole('option', { name: 'routed' })).toBeNull();
    expect(await screen.findByText(/Restart the daemon/)).toBeDefined();
  });
});
