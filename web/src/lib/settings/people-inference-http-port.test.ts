import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { PeopleInferenceHTTPPort } from './people-inference-http-port';

const profile = {
  name: 'routed', preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one',
  endpoint: 'https://openrouter.example.test/api/v1', credential_source: 'stored',
  credential_configured: true, credential_revision: '"credential-a"', checked: true,
  consent_active: false, fingerprint: 'fingerprint-routed', selected: false,
  output_mode: 'strict_schema', allowed_sources: ['conversation_text'], source_since: '2025-01-01',
  allow_sensitive: true, retention_posture: 'No retention', training_posture: 'No training',
};

describe('PeopleInferenceHTTPPort', () => {
  it.each([[true, 'Signed in'], [false, 'Sign-in needed']] as const)(
    'reports Codex credential availability %s as %s from daemon status', async (configured, expected) => {
      const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
        profiles: [{ ...profile, protocol: 'codex_app_server', credential_source: 'none',
          credential_configured: configured }], configured_enabled: false, running_enabled: false,
        pending_restart: false,
      }, { headers: { ETag: '"config-a"' } }));
      const port = new PeopleInferenceHTTPPort(createAPIClient(fetchFn));
      expect((await port.load()).profiles[0]?.credentialStatus).toBe(expected);
    },
  );

  it('does not send a synthetic check while the daemon reports an environment credential missing', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (request.method !== 'GET') throw new Error('Synthetic check must wait for daemon credential status.');
      return Response.json({ profiles: [{ ...profile, credential_source: 'env',
        credential_env: 'PEOPLE_API_KEY', credential_configured: false }],
      configured_enabled: false, running_enabled: false, pending_restart: false },
      { headers: { ETag: '"config-a"' } });
    });
    const port = new PeopleInferenceHTTPPort(createAPIClient(fetchFn));
    await port.load();
    await expect(port.check('routed')).rejects.toThrow('Set PEOPLE_API_KEY on daemon host');
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('normalizes an unknown generated login state to a failed controller state', async () => {
    const port = new PeopleInferenceHTTPPort(createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ state: 'expired' }))));
    expect(await port.pollLogin!('session-1', new AbortController().signal)).toEqual({
      state: 'failed', message: 'Codex sign-in failed.',
    });
  });

  it('maps owner-bound generated Codex login, polling, model, and cancel routes for enrollment', async () => {
    const requests: Request[] = [];
    let polls = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST' && path.endsWith('/codex/login')) return Response.json({
        session_id: 'session-1', verification_url: 'https://example.test/device',
        user_code: 'ABCD-EFGH', local_deadline: '2026-09-23T09:00:00Z',
      });
      if (request.method === 'GET' && path.endsWith('/session-1')) {
        polls += 1;
        return Response.json({ state: polls === 1 ? 'pending' : 'complete' });
      }
      if (request.method === 'GET' && path.endsWith('/session-1/models')) return Response.json({
        models: [{ id: 'codex-model', display_name: 'Codex model',
          default_reasoning_effort: 'medium', supported_efforts: ['low', 'medium'] }],
      });
      if (request.method === 'DELETE' && path.endsWith('/session-1')) return Response.json({ state: 'cancelled' });
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    const port = new PeopleInferenceHTTPPort(createAPIClient(fetchFn));
    expect(port.supportsCodexEnrollment).toBe(true);
    await expect(port.models!('subscription')).rejects.toThrow('Start Codex sign-in');
    expect(await port.startLogin!('subscription')).toEqual({
      sessionID: 'session-1', url: 'https://example.test/device', code: 'ABCD-EFGH',
      localDeadline: '2026-09-23T09:00:00Z',
    });
    const signal = new AbortController().signal;
    expect(await port.pollLogin!('session-1', signal)).toEqual({ state: 'pending' });
    expect(await port.pollLogin!('session-1', signal)).toEqual({ state: 'complete' });
    expect(await port.models!('subscription')).toEqual([{ id: 'codex-model', reasoningEfforts: ['low', 'medium'] }]);
    await port.cancelLogin!('session-1');
    await expect(port.models!('subscription')).rejects.toThrow('Start Codex sign-in');
    expect(requests.map((item) => [item.method, new URL(item.url).pathname])).toEqual([
      ['POST', '/api/v1/settings/people-inference/codex/login'],
      ['GET', '/api/v1/settings/people-inference/codex/login/session-1'],
      ['GET', '/api/v1/settings/people-inference/codex/login/session-1'],
      ['GET', '/api/v1/settings/people-inference/codex/login/session-1/models'],
      ['DELETE', '/api/v1/settings/people-inference/codex/login/session-1'],
    ]);
    await expect(requests[0]!.clone().json()).resolves.toEqual({ name: 'subscription' });
  });

  it('uses the generated check route and server profile for the exact disclosure', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      requests.push(request);
      if (request.method === 'GET') return Response.json({ profiles: [profile], configured_enabled: false,
        running_enabled: false, pending_restart: false }, { headers: { ETag: '"config-a"' } });
      return Response.json({ ok: true, fingerprint: 'fingerprint-routed', model: 'model-one', usage: {} });
    });
    const port = new PeopleInferenceHTTPPort(createAPIClient(fetchFn));
    expect((await port.load()).profiles[0]?.credentialStatus).toBe('Stored key');
    expect(await port.check('routed')).toEqual({
      fingerprint: 'fingerprint-routed', disclosure: {
        sourceClasses: ['conversation_text'], since: '2025-01-01', until: undefined,
        sensitiveContent: true, endpoint: profile.endpoint, retention: 'No retention', training: 'No training',
      },
    });
    expect(requests[1]?.headers.get('If-Match')).toBe('"config-a"');
  });
});
