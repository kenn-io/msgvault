import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import type { PeopleCodexProfileRequest, PeopleInferencePresetCreateRequest,
  PeopleInferenceProfileSetting } from '../api/generated/models';
import { PeopleInferenceAPI, PeopleInferenceConflictError } from './people-inference-api';

const emptyStatus = {
  profiles: [], configured_enabled: false, running_enabled: false, pending_restart: false,
};

const preset: PeopleInferencePresetCreateRequest = {
  preset_id: 'openrouter', model: 'model-one', allowed_sources: ['messages'],
  source_since: '2025-01-01', source_until: '2025-12-31', allow_sensitive: false,
  retention_posture: 'Operator assertion: no retention',
  training_posture: 'Operator assertion: no training',
};

const checkedProfile: PeopleInferenceProfileSetting = {
  name: 'routed', preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one',
  credential_source: 'stored', credential_configured: true, credential_revision: '"credential-a"',
  checked: true, consent_active: false, fingerprint: 'fingerprint-routed',
  output_mode: 'strict_schema', retention_posture: preset.retention_posture,
  training_posture: preset.training_posture, allowed_sources: preset.allowed_sources,
  source_since: preset.source_since, allow_sensitive: false, selected: false,
  endpoint: 'https://openrouter.example.test/api/v1',
};

describe('PeopleInferenceAPI', () => {
  it('writes a completed Codex draft with config ETag and invalidates stale revisions', async () => {
    const request: PeopleCodexProfileRequest = {
      model: 'codex-model', reasoning_effort: 'medium', allowed_sources: ['conversation_text'],
      source_since: '2025-01-01', allow_sensitive: true,
      retention_posture: 'No retention', training_posture: 'No training',
    };
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const item = input as Request;
      requests.push(item);
      if (item.method === 'GET') return Response.json(emptyStatus, { headers: { ETag: '"config-a"' } });
      return Response.json({ error: 'settings_conflict' }, { status: 412 });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));
    await api.load();
    await expect(api.createCodexProfile('session-1', request)).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(api.createCodexProfile('session-1', request)).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(2);
    expect(new URL(requests[1]!.url).pathname).toBe('/api/v1/settings/people-inference/codex/login/session-1/profile');
    expect(requests[1]!.headers.get('If-Match')).toBe('"config-a"');
    await expect(requests[1]!.clone().json()).resolves.toEqual(request);
  });

  it('uses generated GET, preset creation, and selection with the latest config ETag', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (requests.length === 1) return Response.json(emptyStatus, { headers: { ETag: '"config-a"' } });
      if (requests.length === 2) return Response.json({ ...emptyStatus, profiles: [{
        name: 'routed', protocol: 'openai-chat', model: 'model-one', credential_source: 'none',
        output_mode: 'strict_schema', retention_posture: preset.retention_posture,
        training_posture: preset.training_posture, allowed_sources: preset.allowed_sources,
        source_since: preset.source_since, allow_sensitive: false, selected: false,
      }] }, { headers: { ETag: '"config-b"' } });
      return Response.json({ ...emptyStatus, configured_name: 'routed', configured_enabled: true,
        pending_restart: true }, { headers: { ETag: '"config-c"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    expect(await api.load()).toEqual(emptyStatus);
    const created = await api.createPreset('routed', preset);
    expect(created.profiles[0]?.name).toBe('routed');
    expect(await api.select('routed')).toMatchObject({ configured_name: 'routed', pending_restart: true });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/v1/settings/people-inference'],
      ['PUT', '/api/v1/settings/people-inference/providers/routed'],
      ['POST', '/api/v1/settings/people-inference/select'],
    ]);
    expect(requests[1]?.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[2]?.headers.get('If-Match')).toBe('"config-b"');
    await expect(requests[1]?.clone().json()).resolves.toEqual(preset);
    await expect(requests[2]?.clone().json()).resolves.toEqual({ name: 'routed' });
  });

  it('rejects stale writes and requires a fresh read before retrying', async () => {
    let reads = 0;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') {
        reads += 1;
        return Response.json(emptyStatus, { headers: { ETag: reads === 1 ? '"old"' : '"current"' } });
      }
      if (request.headers.get('If-Match') === '"old"') {
        return Response.json({ error: 'settings_conflict' }, { status: 412 });
      }
      return Response.json(emptyStatus, { headers: { ETag: '"next"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await expect(api.createPreset('routed', preset)).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(0);
    await api.load();
    await expect(api.createPreset('routed', preset)).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(api.createPreset('routed', preset)).rejects.toThrow('Load people sweep settings');
    await api.load();
    await api.createPreset('routed', preset);
    expect(requests.at(-1)?.headers.get('If-Match')).toBe('"current"');
  });

  it('requires a new status read after a stale selection', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json(emptyStatus, { headers: { ETag: '"old"' } });
      return Response.json({ error: 'settings_conflict' }, { status: 412 });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.select('routed')).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(api.select('routed')).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(2);
  });

  it('uses the profile credential revision for write-only keys, separately from config ETag', async () => {
    const requests: Request[] = [];
    const profile = (revision: string) => ({
      name: 'routed', preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one',
      credential_source: 'stored', credential_configured: true, credential_revision: revision,
      output_mode: 'strict_schema', retention_posture: preset.retention_posture,
      training_posture: preset.training_posture, allowed_sources: preset.allowed_sources,
      source_since: preset.source_since, allow_sensitive: false, selected: false,
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const revision = requests.filter((item) => item.method === 'PUT').length === 1
        ? '"credential-b"' : '"credential-c"';
      return Response.json({ ...emptyStatus, profiles: [profile(request.method === 'GET' ? '"credential-a"' : revision)] }, {
        headers: { ETag: '"config-a"' },
      });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    const first = await api.setKey('routed', 'synthetic-secret');
    expect(first.profiles[0]?.credential_configured).toBe(true);
    expect(first.pending_restart).toBe(false);
    expect(JSON.stringify(first)).not.toContain('synthetic-secret');
    await api.setKey('routed', 'replacement-secret');
    await api.select('routed');
    expect(requests.map((request) => request.method)).toEqual(['GET', 'PUT', 'PUT', 'POST']);
    expect(requests[1]?.headers.get('If-Match')).toBe('"credential-a"');
    expect(requests[2]?.headers.get('If-Match')).toBe('"credential-b"');
    expect(requests[3]?.headers.get('If-Match')).toBe('"config-a"');
    await expect(requests[1]?.clone().json()).resolves.toEqual({ value: 'synthetic-secret' });
    expect(new URL(requests[1]!.url).pathname).toBe('/api/v1/settings/people-inference/providers/routed/key');
  });

  it('fails clearly when GET omits the config ETag and prevents a write', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(emptyStatus);
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await expect(api.load()).rejects.toThrow('config ETag');
    await expect(api.createPreset('routed', preset)).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(1);
  });

  it('invalidates only the key revision after a stale credential write', async () => {
    const requests: Request[] = [];
    const status = { ...emptyStatus, profiles: [{
      name: 'routed', preset_id: 'openrouter', credential_revision: '"credential-a"',
      credential_configured: false, credential_source: 'stored', protocol: 'openai-chat',
      model: 'model-one', output_mode: 'strict_schema', retention_posture: preset.retention_posture,
      training_posture: preset.training_posture, allowed_sources: preset.allowed_sources,
      source_since: preset.source_since, allow_sensitive: false, selected: false,
    }] };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'PUT') return Response.json({ error: 'credential_conflict' }, { status: 412 });
      return Response.json(status, { headers: { ETag: '"config-a"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.setKey('routed', 'synthetic-secret')).rejects.toThrow('credential changed');
    await expect(api.setKey('routed', 'synthetic-secret')).rejects.toThrow('Reload people sweep settings');
    await api.select('routed');
    expect(requests.map((request) => request.method)).toEqual(['GET', 'PUT', 'POST']);
    expect(requests[2]?.headers.get('If-Match')).toBe('"config-a"');
  });

  it('uses generated check and consent routes with exact fingerprint and config ETag', async () => {
    const requests: Request[] = [];
    const status = { ...emptyStatus, profiles: [checkedProfile] };
    const consented = { ...status, profiles: [{ ...checkedProfile, consent_active: true }] };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET') return Response.json(status, { headers: { ETag: '"config-a"' } });
      if (path.endsWith('/check')) return Response.json({
        ok: true, fingerprint: 'fingerprint-routed', model: 'model-one',
        usage: { input_tokens: 2, output_tokens: 1 },
      });
      if (path.endsWith('/consent')) return Response.json(consented, { headers: { ETag: '"config-b"' } });
      return Response.json({ ...consented, configured_name: 'routed' }, { headers: { ETag: '"config-c"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.check('routed')).resolves.toMatchObject({ ok: true, fingerprint: 'fingerprint-routed' });
    const result = await api.consent('routed', 'fingerprint-routed', true);
    expect(result.profiles[0]).toMatchObject({ fingerprint: 'fingerprint-routed', consent_active: true });
    await api.select('routed');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/v1/settings/people-inference'],
      ['POST', '/api/v1/settings/people-inference/providers/routed/check'],
      ['POST', '/api/v1/settings/people-inference/providers/routed/consent'],
      ['POST', '/api/v1/settings/people-inference/select'],
    ]);
    expect(requests[1]?.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[2]?.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[3]?.headers.get('If-Match')).toBe('"config-b"');
    await expect(requests[2]?.clone().json()).resolves.toEqual({
      fingerprint: 'fingerprint-routed', confirmed: true,
    });
    await expect(requests[1]?.clone().text()).resolves.toBe('');
  });

  it('rejects a checked fingerprint that differs from the server profile', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'GET') return Response.json({ ...emptyStatus, profiles: [checkedProfile] }, {
        headers: { ETag: '"config-a"' },
      });
      return Response.json({ ok: true, fingerprint: 'fingerprint-other', model: 'model-one',
        usage: { input_tokens: 2, output_tokens: 1 } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.check('routed')).rejects.toThrow('did not match');
    await expect(api.check('routed')).rejects.toThrow('Load people sweep settings');
  });

  it.each(['check', 'consent'] as const)('requires a fresh status read after stale %s', async (operation) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ ...emptyStatus, profiles: [checkedProfile] }, {
        headers: { ETag: '"config-a"' },
      });
      return Response.json({ error: 'settings_conflict' }, { status: 412 });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));
    const act = () => operation === 'check' ? api.check('routed') : api.consent('routed', 'fingerprint-routed', true);

    await api.load();
    await expect(act()).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(act()).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(2);
  });

  it('rejects consent that the returned server status does not record', async () => {
    const status = { ...emptyStatus, profiles: [checkedProfile] };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return Response.json(status, { headers: { ETag: '"config-a"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.consent('routed', 'fingerprint-routed', true)).rejects.toThrow('Consent was not recorded');
  });

  it('does not send consent without explicit disclosure confirmation', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({ ...emptyStatus, profiles: [checkedProfile] }, {
        headers: { ETag: '"config-a"' },
      });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.consent('routed', 'fingerprint-routed', false)).rejects.toThrow('Confirm the exact disclosure');
    expect(requests).toHaveLength(1);
  });

  it('uses generated revoke and disable routes with config ETag and exact profile fingerprint', async () => {
    const requests: Request[] = [];
    const active = { ...emptyStatus, configured_enabled: true, running_enabled: true,
      profiles: [{ ...checkedProfile, consent_active: true }] };
    const revoked = { ...active, profiles: [checkedProfile] };
    const disabled = { ...revoked, configured_enabled: false, pending_restart: true };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET') return Response.json(active, { headers: { ETag: '"config-a"' } });
      if (path.endsWith('/revoke')) return Response.json(revoked, { headers: { ETag: '"config-a"' } });
      return Response.json(disabled, { headers: { ETag: '"config-b"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.revoke('routed', 'fingerprint-other')).rejects.toThrow('fingerprint');
    expect(requests).toHaveLength(1);
    expect((await api.revoke('routed', 'fingerprint-routed')).profiles[0]?.consent_active).toBe(false);
    expect(await api.disable()).toMatchObject({ configured_enabled: false, running_enabled: true, pending_restart: true });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/v1/settings/people-inference'],
      ['POST', '/api/v1/settings/people-inference/providers/routed/revoke'],
      ['POST', '/api/v1/settings/people-inference/disable'],
    ]);
    expect(requests[1]?.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[2]?.headers.get('If-Match')).toBe('"config-a"');
    await expect(requests[1]?.clone().text()).resolves.toBe('');
    await expect(requests[2]?.clone().text()).resolves.toBe('');
  });

  it.each(['revoke', 'disable'] as const)('requires a fresh status read after stale %s', async (operation) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ ...emptyStatus, profiles: [checkedProfile] }, {
        headers: { ETag: '"config-a"' },
      });
      return Response.json({ error: 'settings_conflict' }, { status: 412 });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));
    const act = () => operation === 'revoke' ? api.revoke('routed', 'fingerprint-routed') : api.disable();

    await api.load();
    await expect(act()).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(act()).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(2);
  });

  it('removes a provider through the generated route with its config ETag', async () => {
    const requests: Request[] = [];
    const spare = { ...checkedProfile, name: 'spare', fingerprint: 'fingerprint-spare' };
    const before = { ...emptyStatus, profiles: [checkedProfile, spare] };
    const after = { ...emptyStatus, profiles: [spare], pending_restart: true };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json(before, { headers: { ETag: '"config-a"' } });
      if (request.method === 'DELETE') return Response.json(after, { headers: { ETag: '"config-b"' } });
      return Response.json({ ...after, configured_enabled: false }, { headers: { ETag: '"config-c"' } });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    expect(await api.remove('routed')).toEqual(after);
    await api.disable();

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/v1/settings/people-inference'],
      ['DELETE', '/api/v1/settings/people-inference/providers/routed'],
      ['POST', '/api/v1/settings/people-inference/disable'],
    ]);
    expect(requests[1]?.headers.get('If-Match')).toBe('"config-a"');
    expect(requests[2]?.headers.get('If-Match')).toBe('"config-b"');
    await expect(requests[1]?.clone().text()).resolves.toBe('');
  });

  it('requires a fresh read after stale removal', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ ...emptyStatus, profiles: [checkedProfile] }, {
        headers: { ETag: '"config-a"' },
      });
      return Response.json({ error: 'settings_conflict' }, { status: 412 });
    });
    const api = new PeopleInferenceAPI(createAPIClient(fetchFn));

    await api.load();
    await expect(api.remove('routed')).rejects.toBeInstanceOf(PeopleInferenceConflictError);
    await expect(api.remove('routed')).rejects.toThrow('Load people sweep settings');
    expect(requests).toHaveLength(2);
  });
});
