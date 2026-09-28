import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import type { PeopleInferenceProfileSetting, PeopleInferenceSettingsResponse } from '../../api/generated/models';
import { PeopleInferenceController } from '../../settings/people-inference-controller.svelte';
import PeopleInferenceSettings from './PeopleInferenceSettings.svelte';

const profile: PeopleInferenceProfileSetting = {
  name: 'routed', preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one',
  endpoint: 'https://openrouter.example.test/api/v1', credential_source: 'stored',
  credential_configured: true, credential_revision: '"credential-a"', checked: false,
  consent_active: false, fingerprint: 'fingerprint-routed', selected: false,
  output_mode: 'strict_schema', allowed_sources: ['conversation_text'], source_since: '2025-01-01',
  allow_sensitive: true, retention_posture: 'No retention', training_posture: 'No training',
};
const status: PeopleInferenceSettingsResponse = {
  stored_credentials_supported: true, profiles: [profile], configured_enabled: false, running_enabled: false, pending_restart: false,
};
const preset = {
  preset_id: 'openrouter' as const, model: 'model-one', allowed_sources: ['conversation_text'],
  source_since: '2025-01-01', allow_sensitive: true, retention_posture: 'No retention', training_posture: 'No training',
};
function fixture(initial = status, respond?: (request: Request) => Response | Promise<Response | undefined> | undefined) {
  let current = structuredClone(initial);
  const requests: Request[] = [];
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    requests.push(request.clone());
    const custom = await respond?.(request.clone());
    if (custom) return custom;
    const path = new URL(request.url).pathname;
    if (request.method === 'GET') return Response.json(current, { headers: { ETag: '"config-a"' } });
    if (path.endsWith('/check')) {
      current = { ...current, profiles: current.profiles.map((item) => ({ ...item, checked: true })) };
      return Response.json({ ok: true, fingerprint: 'fingerprint-routed' });
    }
    if (path.endsWith('/consent')) {
      current = { ...current, profiles: current.profiles.map((item) => ({ ...item, consent_active: true })) };
    } else if (path.endsWith('/revoke')) {
      current = { ...current, profiles: current.profiles.map((item) => ({ ...item, consent_active: false })) };
    } else if (path.endsWith('/select')) {
      current = { ...current, configured_name: 'routed', configured_enabled: true, pending_restart: true };
    } else if (path.endsWith('/disable')) {
      current = { ...current, configured_enabled: false, pending_restart: true };
    } else if (path.endsWith('/key')) {
      current = { ...current, profiles: current.profiles.map((item) => ({ ...item,
        credential_revision: '"credential-b"', credential_configured: true, checked: false, consent_active: false })) };
    } else if (request.method === 'DELETE') {
      current = { ...current, profiles: current.profiles.filter((item) => !path.endsWith(`/${item.name}`)) };
    } else if (request.method === 'PUT' && path.endsWith('/routed')) {
      current = { ...current, profiles: [{ ...profile, credential_configured: false }] };
    } else throw new Error(`Unexpected request: ${request.method} ${path}`);
    return Response.json(current, { headers: { ETag: '"config-b"' } });
  }));
  return { client, requests };
}

async function checkAndConsent(controller: PeopleInferenceController): Promise<void> {
  await controller.check();
  controller.disclosureConfirmed = true;
  await controller.consent();
}

describe('PeopleInferenceSettings', () => {
  it('hides stored-key enrollment on unsupported hosts while retaining host-managed profiles', async () => {
    const { client, requests } = fixture({ ...status, stored_credentials_supported: false,
      profiles: [{ ...profile, credential_source: 'env', credential_env: 'PEOPLE_API_KEY' }] });
    render(PeopleInferenceSettings, { client });
    await screen.findByText(/Configure an environment credential with/);
    expect(screen.getByText('msgvault person provider add')).toBeDefined();
    expect(screen.getByText('--credential-env')).toBeDefined();
    expect(screen.getByText(/on the daemon host/)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Create profile' })).toBeNull();
    expect(screen.queryByLabelText('API key')).toBeNull();
    expect(screen.queryByLabelText('Replacement API key')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await screen.findByRole('region', { name: 'Archive disclosure' });
    expect(requests.some((request) => request.url.endsWith('/check'))).toBe(true);
    expect(requests.some((request) => request.method === 'PUT')).toBe(false);
  });

  it('hides replacement keys for saved stored-key profiles on unsupported hosts', async () => {
    const { client } = fixture({ ...status, stored_credentials_supported: false,
      profiles: [{ ...profile, credential_configured: false, credential_revision: undefined }] });
    render(PeopleInferenceSettings, { client });
    await screen.findByText(/Configure an environment credential with/);
    expect(screen.queryByLabelText('Replacement API key')).toBeNull();
    expect((screen.getByRole('button', { name: 'Check provider' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('keeps a newly saved profile visible when saving its key fails and lets the user retry', async () => {
    let failKey = true;
    const { client, requests } = fixture({ ...status, profiles: [] }, (request) => {
      if (request.url.endsWith('/key') && failKey) return Response.json({ message: 'Credential store unavailable' }, { status: 500 });
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await controller.create('routed', preset, 'synthetic-secret');
    expect(controller.error).toBe('Credential store unavailable');
    expect(controller.selectedProfile?.name).toBe('routed');
    expect(controller.selectedProfile?.credential_configured).toBe(false);
    expect(requests[1]!.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[2]!.headers.get('If-Match')).toBe('"credential-a"');
    await expect(requests[1]!.json()).resolves.toEqual(preset);
    failKey = false;
    await controller.saveKey('synthetic-replacement');
    expect(controller.error).toBe('');
    expect(controller.selectedProfile?.credential_configured).toBe(true);
  });

  it('requires the checked disclosure and consent before enabling, and shows saved versus running state', async () => {
    const { client, requests } = fixture();
    render(PeopleInferenceSettings, { client });
    await screen.findByText('Stored key');
    expect((screen.getByRole('button', { name: 'Select and enable' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Check provider' }));
    await screen.findByText('Retention: No retention');
    expect((screen.getByRole('button', { name: 'Grant consent' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(screen.getByLabelText('I confirm this exact disclosure'));
    await fireEvent.click(screen.getByRole('button', { name: 'Grant consent' }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Select and enable' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Select and enable' }));
    expect((await screen.findByRole('status')).textContent).toContain('Restart the daemon');
    const consent = requests.find((request) => request.url.endsWith('/consent'))!;
    await expect(consent.json()).resolves.toEqual({ fingerprint: 'fingerprint-routed', confirmed: true });
    expect(consent.headers.get('If-Match')).toBe('"config-a"');
    expect(requests.at(-1)!.headers.get('If-Match')).toBe('"config-b"');
  });

  it('submits explicit archive policy with a write-only API key through the generated client', async () => {
    const { client, requests } = fixture({ ...status, profiles: [] });
    render(PeopleInferenceSettings, { client });
    await screen.findByLabelText('Profile name');
    await fireEvent.click(screen.getByRole('combobox', { name: /^Provider:/ }));
    await fireEvent.click(screen.getByRole('option', { name: 'OpenRouter' }));
    await fireEvent.input(screen.getByLabelText('Profile name'), { target: { value: 'routed' } });
    await fireEvent.input(screen.getByLabelText('Model ID'), { target: { value: 'model-one' } });
    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'synthetic-secret' } });
    await fireEvent.click(screen.getByLabelText('Conversation text'));
    await fireEvent.input(screen.getByLabelText('Archive data since (YYYY-MM-DD)'), { target: { value: '2025-01-01' } });
    await fireEvent.input(screen.getByLabelText('Retention statement'), { target: { value: 'No retention' } });
    await fireEvent.input(screen.getByLabelText('Training statement'), { target: { value: 'No training' } });
    await fireEvent.click(screen.getByLabelText('Allow sensitive content'));
    await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
    await screen.findByText('Stored key');
    await expect(requests[1]!.json()).resolves.toEqual(preset);
    await expect(requests[2]!.json()).resolves.toEqual({ value: 'synthetic-secret' });
    expect((screen.getByLabelText('API key') as HTMLInputElement).value).toBe('');
  });

  it('clears the checked disclosure when a repeat check fails and shows the daemon error', async () => {
    let failCheck = false;
    const { client } = fixture(status, (request) => {
      if (request.url.endsWith('/check') && failCheck) return Response.json({ message: 'Provider rejected the model' }, { status: 422 });
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await checkAndConsent(controller);
    expect(controller.canSelect).toBe(true);
    failCheck = true;
    await controller.check();
    expect(controller.canSelect).toBe(false);
    expect(controller.checkedProfile).toBeUndefined();
    expect(controller.error).toBe('Provider rejected the model');
  });

  it('requires renewed disclosure confirmation even when saved consent is active', async () => {
    const { client } = fixture({ ...status, profiles: [{ ...profile, consent_active: true }] });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await controller.check();
    expect(controller.canSelect).toBe(false);
    controller.disclosureConfirmed = true;
    await controller.consent();
    expect(controller.canSelect).toBe(true);
  });

  it('rejects a check when the refreshed fingerprint differs', async () => {
    let reads = 0;
    const { client } = fixture(status, (request) => {
      if (request.method === 'GET' && ++reads > 1) return Response.json({ ...status,
        profiles: [{ ...profile, fingerprint: 'changed' }] }, { headers: { ETag: '"config-b"' } });
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await controller.check();
    expect(controller.checkedProfile).toBeUndefined();
    expect(controller.error).toContain('changed after the check');
  });

  it('drops a disclosure when selection changes while the check is running', async () => {
    let finish!: (response: Response) => void;
    const checked = new Promise<Response>((resolve) => { finish = resolve; });
    const { client } = fixture({ ...status, profiles: [profile, { ...profile, name: 'spare' }] }, (request) =>
      request.url.endsWith('/check') ? checked : undefined);
    const controller = new PeopleInferenceController(client);
    await controller.load();
    const checking = controller.check();
    controller.choose('spare');
    finish(Response.json({ ok: true, fingerprint: 'fingerprint-routed' }));
    await checking;
    expect(controller.selectedName).toBe('spare');
    expect(controller.checkedProfile).toBeUndefined();
  });

  it('requires reload after a config conflict or a missing ETag', async () => {
    let missingETag = false;
    const { client, requests } = fixture(status, (request) => {
      if (request.method !== 'GET') return Response.json({ message: 'Stale settings' }, { status: 412 });
      if (missingETag) return Response.json(status);
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await controller.disable();
    expect(controller.error).toContain('changed on disk');
    await controller.disable();
    expect(controller.error).toContain('Reload');
    expect(requests).toHaveLength(2);
    missingETag = true;
    await controller.load();
    expect(controller.error).toContain('config ETag');
    await controller.disable();
    expect(requests).toHaveLength(3);
  });

  it('uses credential revisions for key replacement and clears checked consent', async () => {
    const { client, requests } = fixture();
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await checkAndConsent(controller);
    await controller.saveKey('synthetic-secret');
    expect(controller.canSelect).toBe(false);
    await controller.saveKey('synthetic-replacement');
    const keys = requests.filter((request) => request.url.endsWith('/key'));
    expect(keys.map((request) => request.headers.get('If-Match'))).toEqual(['"credential-a"', '"credential-b"']);
  });

  it('requires reload after a credential conflict without invalidating config edits', async () => {
    const { client, requests } = fixture(status, (request) => {
      if (request.url.endsWith('/key')) return Response.json({ message: 'Stale key' }, { status: 412 });
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await controller.saveKey('synthetic-secret');
    expect(controller.error).toContain('credential changed');
    await controller.saveKey('synthetic-replacement');
    expect(requests).toHaveLength(2);
    await controller.disable();
    expect(requests.at(-1)!.headers.get('If-Match')).toBe('"config-a"');
  });

  it('does not enable a profile if consent is missing from the server response', async () => {
    const { client } = fixture(status, (request) => {
      if (request.url.endsWith('/consent')) return Response.json(status, { headers: { ETag: '"config-a"' } });
    });
    const controller = new PeopleInferenceController(client);
    await controller.load();
    await checkAndConsent(controller);
    expect(controller.canSelect).toBe(false);
    expect(controller.error).toContain('Consent was not recorded');
  });

  it('shows host-managed credentials and unavailable Codex profiles without actionable checks', async () => {
    const { client } = fixture({ ...status, profiles: [{ ...profile,
      credential_source: 'env', credential_env: 'PEOPLE_API_KEY', credential_configured: false }] });
    const rendered = render(PeopleInferenceSettings, { client });
    await screen.findByText('Set PEOPLE_API_KEY on daemon host');
    expect((screen.getByRole('button', { name: 'Check provider' }) as HTMLButtonElement).disabled).toBe(true);
    rendered.unmount();
    const codex = fixture({ ...status, profiles: [{ ...profile, protocol: 'codex_app_server' }] });
    render(PeopleInferenceSettings, { client: codex.client });
    await screen.findByText('Codex is unavailable in this release');
    expect((screen.getByRole('button', { name: 'Check provider' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('revokes consent, disables sweeps, and confirms removal when another profile remains', async () => {
    const { client, requests } = fixture({ ...status, configured_name: 'routed', configured_enabled: true,
      running_name: 'routed', running_enabled: true,
      profiles: [{ ...profile, consent_active: true }, { ...profile, name: 'backup' }] });
    render(PeopleInferenceSettings, { client });
    await screen.findByText('Granted for this profile');
    await fireEvent.click(screen.getByRole('button', { name: 'Revoke consent' }));
    await screen.findByText('Not granted');
    await fireEvent.click(screen.getByRole('button', { name: 'Disable people sweep' }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Remove profile' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Remove profile' }));
    expect(requests.some((request) => request.method === 'DELETE')).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: /^Profile:/ }).textContent).toContain('backup'));
    expect(requests.at(-1)!.method).toBe('DELETE');
  });
});
