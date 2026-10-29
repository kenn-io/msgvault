import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SettingsWorkspace from './SettingsWorkspace.svelte';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';

type Setting = Record<string, unknown>;

function typed(value: unknown): Record<string, unknown> {
  if (typeof value === 'boolean') return { boolean: value };
  if (typeof value === 'number') return Number.isInteger(value) ? { integer: value } : { number: value };
  if (Array.isArray(value)) return { strings: value };
  return { string: value };
}

function setting(key: string, group: string, kind: string, value: unknown, extra: Setting = {}): Setting {
  return {
    key, group, kind, label: key, restart_required: false,
    value: value === undefined ? undefined : typed(value), ...extra
  };
}

function settingsDoc(overrides: Setting = {}) {
  return {
    groups: [],
    settings: [
      setting('web.theme', 'browser', 'string', 'system', {
        label: 'Theme', options: ['system', 'light', 'dark'] }),
      setting('web.density', 'browser', 'string', 'compact', {
        label: 'Density', options: ['compact', 'comfortable'] }),
      setting('web.default_search_mode', 'browser', 'string', 'full_text', {
        label: 'Default search mode', options: ['full_text', 'semantic', 'hybrid'] }),
      setting('server.bind_addr', 'server', 'string', '127.0.0.1', { label: 'Bind address', read_only: true }),
      setting('server.api_key', 'server', 'secret', undefined, {
        label: 'API key', read_only: true, secret: { configured: true, source: 'environment', hint: 'abc…xyz' } }),
      setting('server.daemon_idle_timeout', 'server', 'string', '20m0s', { label: 'Idle timeout' }),
      setting('log.sql_trace', 'server', 'boolean', false, { label: 'Trace every SQL statement' }),
      setting('activity.timezone', 'archive', 'string', 'UTC', { label: 'Timezone' }),
      setting('analytics.builder_threads', 'archive', 'integer', 4, { label: 'Builder threads' }),
      setting('sync.rate_limit_qps', 'sources', 'number', 5, { label: 'Requests per second' }),
      setting('vector.enabled', 'search', 'boolean', false, { label: 'Semantic search' }),
      setting('vector.embeddings.endpoint', 'search', 'string', '', { label: 'Text embedding endpoint' }),
      setting('vector.embeddings.model', 'search', 'string', '', { label: 'Text embedding model' }),
      setting('vector.embeddings.api_key', 'search', 'secret', undefined, {
        label: 'Text embedding API key', credential_id: 'vector.embeddings',
        secret: { configured: false, source: 'none' } }),
      setting('vector.search.rrf_k', 'search', 'integer', 60, { label: 'RRF constant' }),
      setting('integrations.kata.enabled', 'integrations', 'boolean', false, { label: 'Kata person agendas' }),
      setting('integrations.kata.api_key', 'integrations', 'secret', undefined, {
        label: 'Kata API key', secret: { configured: false, source: 'none' } }),
      setting('integrations.tasks.enabled', 'integrations', 'boolean', true, { label: 'Task integration' }),
      setting('beeper.schedule', 'sources', 'string', '', { label: 'Beeper schedule' }),
      setting('discord.media', 'attachments', 'boolean', true, { label: 'Download Discord attachments' }),
      setting('people.enrichment.enabled', 'enrichment', 'boolean', false, { label: 'Enable person enrichment' }),
    ],
    person_enrichment_providers: [],
    credential_etag: '"credential-a"',
    pending_restart: false,
    ...overrides
  };
}

interface Seen { method: string; path: string; ifMatch: string | null; body?: unknown }

function fixture(doc: ReturnType<typeof settingsDoc> = settingsDoc(), extra?: (request: Request) => Response | undefined | Promise<Response | undefined>) {
  const seen: Seen[] = [];
  let etag = 1;
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    const path = new URL(request.url).pathname;
    const body = request.method === 'GET' || request.method === 'DELETE' ? undefined : await request.clone().json();
    seen.push({ method: request.method, path, ifMatch: request.headers.get('If-Match'), body });
    const custom = await extra?.(request);
    if (custom) return custom;
    if (path === '/api/v1/settings' && request.method === 'GET') {
      return Response.json(doc, { headers: { ETag: `"s${etag}"`, 'Credential-ETag': '"credential-a"' } });
    }
    if (path === '/api/v1/settings' && request.method === 'PATCH') {
      for (const update of (body as { updates: { key: string; value?: Record<string, unknown> }[] }).updates) {
        const target = (doc.settings as Setting[]).find((candidate) => candidate.key === update.key);
        if (target && update.value) target.value = update.value;
      }
      etag += 1;
      return Response.json({ ...doc, pending_restart: true }, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/person-enrichment/providers/') && request.method === 'PUT') {
      doc.person_enrichment_providers = (doc.person_enrichment_providers as Record<string, unknown>[]).map(provider => path.endsWith('/' + provider.name) ? { ...provider, ...body as object } : provider) as never[];
      etag += 1;
      return Response.json(doc, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/provider-credentials/')) {
      return Response.json(
        { credential_id: decodeURIComponent(path.split('/').at(-1)!), state: { configured: true, source: 'stored', hint: '•••' }, pending_restart: false },
        { headers: { ETag: '"credential-b"' } }
      );
    }
    if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [] });
    if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
    if (path.startsWith('/api/v1/carddav')) {
      if (path.endsWith('/connections')) return Response.json({ connections: [] });
      if (path.endsWith('/status')) {
        return Response.json({ configured: false, available: false, credential_configured: false, enabled: false,
          scheduled: false, schedule: '' });
      }
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      return Response.json({ conflicts: [] });
    }
    if (path.includes('/embeddings/status')) return Response.json({}, { status: 503 });
    if (path.includes('/multimodal/status')) return Response.json({}, { status: 503 });
    if (path.includes('/settings/people-inference')) {
      return Response.json({ profiles: [], configured_enabled: false, running_enabled: false, pending_restart: false },
        { headers: { ETag: '"p1"' } });
    }
    return Response.json({}, { status: 404 });
  }));
  return { client, seen };
}

afterEach(() => vi.restoreAllMocks());

describe('SettingsWorkspace', () => {
  it('lists the categories in order, Overview first, and opens the one it is given', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced' });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    const names = [...window.document.querySelectorAll('.kit-settings__nav-item')].map((item) => item.textContent?.trim());
    expect(names).toEqual([
      'Overview', 'Server', 'Search & AI', 'People lookups', 'Tasks', 'Contacts', 'Sources', 'Advanced'
    ]);
    expect(screen.queryByText('Attachments')).toBeNull();
    expect(screen.queryByText('Beeper schedule')).toBeNull();
  });

  it('shows the plain-HTTP warning on Server only', async () => {
    const { client } = fixture();
    const view = render(SettingsWorkspace, { client, category: 'advanced', plainHTTPWarning: true });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    expect(screen.queryByText(/plain HTTP/)).toBeNull();
    await view.rerender({ client, category: 'server', plainHTTPWarning: true });
    expect(await screen.findByText(/plain HTTP/)).toBeDefined();
  });

  it('keeps tuning behind Advanced and host values behind Set in config.toml', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    await screen.findByRole('heading', { name: 'Server' });
    expect(screen.getByLabelText('Stop when idle after')).toBeDefined();
    const advanced = screen.getByText('Advanced', { selector: 'summary' }).closest('details')!;
    expect(within(advanced).getByRole('switch', { name: 'Trace every SQL statement' })).toBeDefined();
    const config = screen.getByText('Set in config.toml').closest('details')!;
    expect(within(config).getByText('Bind address')).toBeDefined();
    expect(within(config).getByText('127.0.0.1')).toBeDefined();
    expect(screen.queryByText('Host-managed')).toBeNull();
    expect(screen.queryByText(/Saved changes apply/)).toBeNull();
  });

  it('keeps archive tuning and source rate limits reachable', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced' });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    const advanced = screen.getByText('Advanced', { selector: 'summary' }).closest('details')!;

    expect(within(advanced).getByLabelText('Builder threads')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Sources' }));
    expect(screen.getByLabelText('Requests per second')).toBeDefined();
    expect(screen.getByRole('switch', { name: 'Download Discord attachments' })).toBeDefined();
  });

  it('saves every change through one Save changes with If-Match and then asks for a restart', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    await fireEvent.click(await screen.findByRole('switch', { name: 'Trace every SQL statement' }));
    expect(screen.getByText('1 unsaved change')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PATCH')).toBe(true));
    const patch = seen.find((entry) => entry.method === 'PATCH')!;
    expect(patch.ifMatch).toBe('"s1"');
    expect(await screen.findByText('Saved. Restart msgvault to apply these changes.')).toBeDefined();
  });

  it('drops a change that is put back to its saved value, and Discard clears the rest', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    const toggle = await screen.findByRole('switch', { name: 'Trace every SQL statement' });
    await fireEvent.click(toggle);
    await fireEvent.click(toggle);
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
    await fireEvent.click(toggle);
    await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
  });

  it('never treats an emptied number as a value and blocks saving it', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'sources' });
    await screen.findByRole('heading', { name: 'Sources', level: 2 });
    const field = screen.getByLabelText('Requests per second') as HTMLInputElement;
    await fireEvent.input(field, { target: { value: '' } });
    expect(screen.getByText(/Enter a number to save/)).toBeDefined();
    expect((screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('holds a typed secret until Save changes, then sends it with the settings', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'tasks' });
    await fireEvent.click(await screen.findByRole('button', { name: 'Add API key' }));
    await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'synthetic-key' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Use API key' }));
    expect(seen.some((entry) => entry.method === 'PATCH')).toBe(false);
    expect(screen.getByText('1 unsaved change')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PATCH')).toBe(true));
    expect(seen.find((entry) => entry.method === 'PATCH')!.body).toEqual({
      updates: [{ key: 'integrations.kata.api_key', secret: { action: 'set', value: 'synthetic-key' } }]
    });
    expect(screen.queryByText('Task integration')).toBeNull();
  });

  it('saves the endpoint first and then the provider key it binds to', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'search' });
    const endpoint = await screen.findByLabelText('Endpoint');
    await fireEvent.input(endpoint, { target: { value: 'https://embed.example.test/v1' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add API key' }));
    await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'provider-secret' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Use API key' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PUT')).toBe(true));
    const writes = seen.filter((entry) => entry.method !== 'GET' && entry.path !== '/api/v1/search/coverage').map((entry) => `${entry.method} ${entry.path}`);
    expect(writes).toEqual([
      'PATCH /api/v1/settings',
      'PUT /api/v1/settings/provider-credentials/vector.embeddings'
    ]);
    expect(seen.find((entry) => entry.method === 'PUT')!.ifMatch).toBe('"credential-a"');
  });

  it('lands a Set up link on the feature switch', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, {
      client, category: 'search',
      navigationTarget: { authority: 'semantic_search', categoryID: 'search', settingKey: 'vector.enabled' }
    });
    await screen.findByRole('heading', { name: 'Search & AI' });
    await waitFor(() => expect((window.document.activeElement as HTMLElement | null)?.dataset['settingKey'])
      .toBe('vector.enabled'));
  });

  it('reports a saved Appearance value, and hides search-mode choices until vector search is ready', async () => {
    const onAppearanceSaved = vi.fn();
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced', onAppearanceSaved });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    expect(screen.getByRole('combobox', { name: /^Search opens in/ })).toBeDefined();
    await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(onAppearanceSaved).toHaveBeenCalledWith({ theme: 'dark' }));
    expect(screen.queryByText(/Use daemon theme/)).toBeNull();
  });



  it('reloads the latest settings after a conflict and keeps what still differs', async () => {
    let conflicts = 1;
    const { client } = fixture(settingsDoc(), (request) => {
      if (request.method === 'PATCH' && conflicts-- > 0) return Response.json({ error: 'conflict' }, { status: 412 });
      return undefined;
    });
    render(SettingsWorkspace, { client, category: 'server' });
    await fireEvent.click(await screen.findByRole('switch', { name: 'Trace every SQL statement' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    expect((await screen.findByRole('alert')).textContent).toContain('The settings changed on disk');
    expect(screen.getByText('1 unsaved change')).toBeDefined();
  });

  it('keeps Contacts account and sync controls reachable', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'carddav' });
    expect(await screen.findByRole('heading', { name: 'Contacts' })).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Add connection' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sync now' })).toBeDefined();
    expect(screen.queryByText(/These save immediately/)).toBeNull();
  });


  it('offers providers and facts under People lookups, with the add form behind a click', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'enrichment' });
    expect(await screen.findByRole('heading', { name: 'People lookups' })).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Add Exa' })).toBeDefined();
    expect(await screen.findByLabelText('Profile name')).toBeDefined();
  });


});


it('keeps provider endpoints and keys across categories, saves endpoints before keys, and retains only a failed key', async () => {
  let failKey = true;
  const provider = {
    name: 'primary', kind: 'exa', enabled: false, endpoint: 'https://old.example.test',
    credential_id: 'people.enrichment.primary', credential: { configured: false, source: 'none' },
    allowed_identifiers: [], target_keys: [], allow_sensitive_targets: false,
    retention_posture: '', training_posture: '', refresh_interval: '24h', request_timeout: '30s',
    poll_interval: '1s', max_job_age: '1h', max_retries: 2, max_requests_per_run: 10, max_requests_per_day: 100
  };
  const { client, seen } = fixture(settingsDoc({ person_enrichment_providers: [provider, { ...provider, name: 'spare', credential_id: 'people.enrichment.spare' }] }), request => {
    if (request.url.endsWith('/provider-credentials/people.enrichment.primary') && failKey)
      return Response.json({ message: 'Key store unavailable' }, { status: 503 });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.input(await screen.findByLabelText('primary endpoint'), { target: { value: 'https://new.example.test' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Add API key for primary' }));
  await fireEvent.input(screen.getByLabelText('New API key for primary'), { target: { value: 'synthetic-provider-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Use API key for primary' }));
  expect(screen.getByLabelText('primary credential').classList.contains('setting--changed')).toBe(true);
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.click(screen.getByRole('switch', { name: 'Trace every SQL statement' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Key store unavailable');
  expect(screen.getByText('1 unsaved change')).toBeDefined();
  const writes = seen.filter(request => request.method !== 'GET' && request.path !== '/api/v1/search/coverage');
  expect(writes.map(({ method, path, ifMatch }) => [method, path, ifMatch])).toEqual([
    ['PATCH', '/api/v1/settings', '"s1"'],
    ['PUT', '/api/v1/settings/person-enrichment/providers/primary', '"s2"'],
    ['PUT', '/api/v1/settings/provider-credentials/people.enrichment.primary', '"credential-a"']
  ]);
  expect(writes[1].body).toMatchObject({ endpoint: 'https://new.example.test' });
  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  expect((await screen.findByLabelText('spare endpoint') as HTMLInputElement).value).toBe('https://old.example.test');
  failKey = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.filter(request => request.method === 'PATCH')).toHaveLength(1);
  expect(seen.filter(request => request.path.endsWith('/providers/primary'))).toHaveLength(1);
});

it('saves account drafts from Overview and drops their passwords on Discard', async () => {
  const { client, seen } = fixture(settingsDoc(), request => {
    if (new URL(request.url).pathname === '/api/v1/carddav/account')
      return Response.json({ base_url: 'https://dav.example.test/', username: 'account@example.com', enabled: false, schedule: '', books: 1 });
  });
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('Base URL'), { target: { value: 'https://dav.example.test/' } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'account@example.com' } });
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-account-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-account-key');
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.filter(request => request.method === 'PUT').map(request => request.body)).toEqual([
    { connection: 'default', base_url: 'https://dav.example.test/', username: 'account@example.com', password: 'synthetic-account-key', enabled: false, schedule: '' }
  ]);
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'discarded-account-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
});

it('retains inference replacement keys across navigation and saves them from the footer', async () => {
  const inference = { profiles: [{ name: 'primary', protocol: 'openai', model: 'example-model', credential_source: 'stored', credential_configured: true, credential_revision: '"key-a"' }], configured_enabled: false, running_enabled: false, pending_restart: false };
  const { client, seen } = fixture(settingsDoc(), request => {
    if (request.url.includes('/settings/people-inference'))
      return Response.json(inference, { headers: { ETag: '"p1"' } });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.input(await screen.findByLabelText('Replacement API key'), { target: { value: 'synthetic-inference-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  expect((await screen.findByLabelText('Replacement API key') as HTMLInputElement).value).toBe('synthetic-inference-key');
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PUT')).toMatchObject({ path: '/api/v1/settings/people-inference/providers/primary/key', ifMatch: '"key-a"', body: { value: 'synthetic-inference-key' } });
});

it('keeps a host-owned primary field read-only', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('server.daemon_auto_start', 'server', 'boolean', true, { read_only: true, label: 'Automatic start' }));
  const { client } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'server' });
  await screen.findByRole('heading', { name: 'Server' });
  expect(screen.queryByRole('switch', { name: 'Start automatically' })).toBeNull();
  expect(screen.getByText('Start automatically')).toBeDefined();
});

it('keeps attachment off values and cron schedules editable under Sources', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('discord.max_media_mb', 'attachments', 'integer', 0, {
    label: 'Maximum attachment size', validation: { minimum: 0, off: { value: '0', label: 'Provider default', suggest: '50', on_minimum: 1 } }
  }));
  doc.settings.push(setting('slack.schedule', 'sources', 'string', '0 3 * * *', { label: 'Slack schedule', validation: { format: 'cron' } }));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'sources' });
  await fireEvent.click(await screen.findByRole('switch', { name: 'Set Maximum attachment size' }));
  expect((screen.getByLabelText('Maximum attachment size') as HTMLInputElement).value).toBe('50');
  await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Every 15 minutes');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [
    { key: 'discord.max_media_mb', value: { integer: 50 } },
    { key: 'slack.schedule', value: { string: '*/15 * * * *' } }
  ] }));
});

it.each(['succeeded', 'partial', 'failed', 'conflict'])('Overview syncs all contacts connections and reports %s', async result => {
  const { client, seen } = fixture(settingsDoc(), request => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/operations/status')) return Response.json({ lanes: [{
      kind: 'carddav_sync', lane: 'contacts', configured: true, supported_actions: ['carddav_sync'],
      latest: { connection: 'work', state: 'succeeded' }
    }] });
    if (path.endsWith('/carddav/sync')) return Response.json({ status: result, connections: [
      { connection: 'work', status: result }, { connection: 'personal', status: 'succeeded' }
    ] }, { status: result === 'conflict' ? 409 : 200 });
  });
  render(SettingsWorkspace, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Sync now, Contacts' }));
  await waitFor(() => expect(seen.find(request => request.path.endsWith('/carddav/sync'))?.body).toEqual({ full: false }));
  if (result === 'succeeded') {
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync now, Contacts' }).hasAttribute('disabled')).toBe(false));
    expect(screen.queryByRole('alert')).toBeNull();
  } else {
    expect((await screen.findByRole('alert')).textContent).toContain(result === 'conflict' ? 'current server state changed' : `CardDAV sync ${result}. Connections needing attention: work.`);
  }
});

it('keeps a newly drafted contact connection selectable across category changes', async () => {
  const { client } = fixture();
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('New connection name'), { target: { value: 'work' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Add connection' }));
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'work@example.com' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV connection/ }), 'work');
  expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('work@example.com');
});

it('validates contact account drafts before writing other settings', async () => {
  const { client, seen } = fixture();
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('Username'), { target: { value: 'work@example.com' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.input(screen.getByLabelText('Stop when idle after'), { target: { value: '15m' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Password is required');
  expect(seen.filter(request => request.method !== 'GET' && request.path !== '/api/v1/search/coverage')).toEqual([]);
  expect(screen.getByText('2 unsaved changes')).toBeDefined();
});

it('saves other categories after leaving a contact connection test', async () => {
  let finish!: (response: Response) => void;
  const testing = new Promise<Response>(resolve => { finish = resolve; });
  const { client, seen } = fixture(settingsDoc(), request => request.url.endsWith('/carddav/account/test') ? testing : undefined);
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('Base URL'), { target: { value: 'https://dav.example.test/' } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'work@example.com' } });
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
  await waitFor(() => expect(seen.some(request => request.path.endsWith('/carddav/account/test'))).toBe(true));
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.input(screen.getByLabelText('Stop when idle after'), { target: { value: '15m' } });
  expect((screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.some(request => request.method === 'PATCH')).toBe(true));
  finish(Response.json({ books: 1 }));
});

it('refreshes Overview after an account save and reuses its saved password on a later edit', async () => {
  let saved = false;
  const values = { configured: true, available: false, enabled: true, credential_configured: true, schedule: '', account: { base_url: 'https://dav.example.test/', username: 'work@example.com' } };
  const { client, seen } = fixture(settingsDoc(), request => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/operations/status')) return Response.json({ lanes: [{
      kind: 'carddav_sync', lane: 'contacts', configured: saved, supported_actions: saved ? ['carddav_sync'] : []
    }] });
    if (path.endsWith('/carddav/account')) {
      saved = true;
      return Response.json({ ...values.account, enabled: true, schedule: '', books: 2 });
    }
    if (saved && path.endsWith('/carddav/connections')) return Response.json({ connections: [{ connection: 'default', status: values }] });
    if (saved && path.endsWith('/carddav/status')) return Response.json(values);
  });
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('Base URL'), { target: { value: values.account.base_url } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: values.account.username } });
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });
  await fireEvent.click(screen.getByRole('switch', { name: 'Enabled' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  expect(await screen.findByRole('button', { name: 'Set up Contacts' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  expect(await screen.findByText('Contact account default saved. Found 2 address books.')).toBeDefined();
  expect(await screen.findByRole('button', { name: 'Sync now, Contacts' })).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Set up Contacts' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  await waitFor(() => expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe(values.account.username));
  await chooseSelectOption(screen.getByRole('combobox', { name: /^Presets:/ }), 'Every day at 03:00');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.filter(request => request.path.endsWith('/carddav/account'))).toHaveLength(2));
  const writes = seen.filter(request => request.path.endsWith('/carddav/account'));
  expect(writes[0].body).toMatchObject({ password: 'synthetic-password' });
  expect(writes[1].body).toEqual({ connection: 'default', base_url: values.account.base_url, username: values.account.username, enabled: true, schedule: '0 3 * * *' });
});

it('retains a conflicting provider key and retries with the refreshed credential revision', async () => {
  let conflict = true;
  const doc = settingsDoc();
  const { client, seen } = fixture(doc, request => {
    if (request.method === 'GET' && request.url.endsWith('/settings')) return Response.json(doc, { headers: {
      ETag: '"config-current"', 'Credential-ETag': conflict ? '"credential-a"' : '"credential-refreshed"'
    } });
    if (request.method === 'PUT' && conflict) {
      conflict = false;
      return Response.json({ message: 'Stale key' }, { status: 412 });
    }
  });
  render(SettingsWorkspace, { client, category: 'search' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Add API key' }));
  await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'synthetic-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Use API key' }));
  expect(document.querySelector('[data-setting-key="vector.embeddings.api_key"]')?.classList.contains('setting--changed')).toBe(true);
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  expect((await screen.findByRole('alert')).textContent).toContain('The key changed elsewhere');
  expect(screen.getByText('1 unsaved change')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.filter(request => request.method === 'PUT').map(request => request.ifMatch)).toEqual(['"credential-a"', '"credential-refreshed"']);
  expect(seen.filter(request => request.method === 'PATCH')).toEqual([]);
});

it('rejects an invalid new provider name before sending a write', async () => {
  const { client, seen } = fixture();
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Add Exa' }));
  await fireEvent.input(screen.getByLabelText('Exa name'), { target: { value: 'bad/name' } });
  await fireEvent.submit(screen.getByRole('form', { name: 'Add Exa' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Names can use letters');
  expect(seen.filter(request => request.method !== 'GET' && request.path !== '/api/v1/search/coverage')).toEqual([]);
});

it('locks provider fields and newly mounted category controls while a provider save waits', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const base = { enabled: false, endpoint: 'https://provider.example.test', credential: { configured: false, source: 'none' }, allowed_identifiers: [], target_keys: [] };
  const doc = settingsDoc({ person_enrichment_providers: [
    { ...base, name: 'primary', kind: 'exa', mode: 'people', credential_id: 'people.enrichment.primary' },
    { ...base, name: 'backup', kind: 'sixtyfour', tier: 'standard', credential_id: 'people.enrichment.backup' }
  ] });
  const { client, seen } = fixture(doc, request => {
    if (request.method === 'PUT') return pending;
    if (request.url.includes('/settings/people-inference')) return Response.json({
      profiles: [{ name: 'routed', protocol: 'openai', credential_source: 'stored', credential_configured: true }],
      configured_enabled: false, running_enabled: false, pending_restart: false
    }, { headers: { ETag: '"p1"' } });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await screen.findByLabelText('Replacement API key');
  await fireEvent.input(await screen.findByLabelText('primary endpoint'), { target: { value: 'https://changed.example.test' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.some(request => request.method === 'PUT')).toBe(true));
  for (const name of ['Search mode', 'Tier', 'Replacement API key']) expect(screen.getByLabelText(name).matches(':disabled'), name).toBe(true);
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect(screen.getByRole('combobox', { name: /^CardDAV connection/ }).matches(':disabled')).toBe(true);
  expect(screen.getByRole('button', { name: 'Add connection' }).matches(':disabled')).toBe(true);
  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  expect((await screen.findByLabelText('Replacement API key')).matches(':disabled')).toBe(true);
  finish(Response.json(doc, { headers: { ETag: '"s2"' } }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Saving…' })).toBeNull());
});

it('shows one footer-save error for an inference key and keeps the key pending', async () => {
  const inference = { profiles: [{ name: 'routed', protocol: 'openai', credential_source: 'stored', credential_configured: true, credential_revision: '"key-a"' }], configured_enabled: false, running_enabled: false, pending_restart: false };
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/key')) return Response.json({ message: 'Key store unavailable' }, { status: 503 });
    if (request.url.includes('/settings/people-inference')) return Response.json(inference, { headers: { ETag: '"p1"' } });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.input(await screen.findByLabelText('Replacement API key'), { target: { value: 'synthetic-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await screen.findAllByRole('alert');
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Saving…' })).toBeNull());
  expect(screen.getAllByRole('alert').map(alert => alert.textContent)).toEqual(['Key store unavailable']);
  expect(screen.getByText('1 unsaved change')).toBeDefined();
});

it.each(['message', 'visual'] as const)('starts the %s index from Overview and refreshes its row', async kind => {
  let built = false;
  const operation = kind === 'message' ? 'message_embedding' : 'visual_embedding';
  const path = kind === 'message' ? '/api/v1/cli/run' : '/api/v1/multimodal/build';
  const { client, seen } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return Response.json({ lanes: [{
      kind: operation, configured: true, history_availability: 'available', supported_actions: kind === 'visual' && !built ? ['visual_build'] : []
    }] });
    if (request.url.includes('/embeddings/status')) return Response.json({ active_generation: built ? 1 : null });
    if (request.url.includes('/search/coverage')) return Response.json({ eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1', status: 'initializing', actions: built ? [] : ['build_index'] });
    if (request.url.endsWith(path)) {
      built = true;
      return kind === 'message' ? new Response('{"type":"complete"}\n') : Response.json({});
    }
  });
  render(SettingsWorkspace, { client });
  const name = kind === 'message' ? 'Build index, Message search' : 'Build visual index, Visual search';
  await fireEvent.click(await screen.findByRole('button', { name }));
  if (kind === 'message') {
    expect(seen.filter(request => request.path === '/api/v1/cli/run')).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(seen.filter(request => request.path === '/api/v1/cli/run')).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
  }
  await waitFor(() => expect(screen.queryByRole('button', { name })).toBeNull());
  expect(seen.find(request => request.path === path)?.body).toEqual(kind === 'message'
    ? { args: ['embeddings', 'build', '--full-rebuild', '--yes'] } : { consent: true });
  expect(within(document.querySelector(`[data-feature="${operation}"]`) as HTMLElement).getByText('On')).toBeDefined();
});

it('refreshes completed feature work while keeping its current rows visible', async () => {
  let finish!: (response: Response) => void;
  let reads = 0;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) {
      reads += 1;
      if (reads === 3) return Response.json({ message: 'Unavailable' }, { status: 503 });
      if (reads > 3) return Response.json({ lanes: [{ kind: 'visual_embedding', configured: true, supported_actions: [] }] });
      return reads === 1 ? Response.json({ lanes: [{ kind: 'visual_embedding', configured: true, active: { state: 'running' }, supported_actions: [] }] }) : pending;
    }
  });
  render(SettingsWorkspace, { client });
  await screen.findByText('Running');
  await waitFor(() => expect((screen.getByRole('button', { name: 'Refresh feature status' }) as HTMLButtonElement).disabled).toBe(false));
  screen.getByRole('button', { name: 'Refresh feature status' }).focus();
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh feature status' }));
  await waitFor(() => expect(reads).toBe(2));
  expect(screen.getByText('Running')).toBeDefined();
  expect((screen.getByRole('button', { name: 'Refresh feature status' }) as HTMLButtonElement).disabled).toBe(true);
  finish(Response.json({ lanes: [{ kind: 'visual_embedding', configured: true, supported_actions: [] }] }));
  await screen.findByText('On');
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Refresh feature status' })));
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh feature status' }));
  await screen.findByText("Can't load status");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByText('On');
});

it.each(['full_text', 'semantic'])('offers approved search defaults and preserves stored %s until a new choice', async stored => {
  const doc = settingsDoc();
  doc.settings.find(item => item.key === 'web.default_search_mode')!.value = typed(stored);
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'advanced' });
  const trigger = await screen.findByRole('combobox', { name: /^Search opens in:/ });
  expect(trigger.textContent).toContain(stored === 'semantic' ? 'Semantic' : 'Exact words');
  expect(screen.queryByText('1 unsaved change')).toBeNull();
  await fireEvent.click(trigger);
  const semantic = screen.queryByRole('option', { name: 'Semantic' });
  if (stored === 'semantic') expect((semantic as HTMLButtonElement).disabled).toBe(true);
  else expect(semantic).toBeNull();
  await fireEvent.click(screen.getByRole('option', { name: 'Best match' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [{ key: 'web.default_search_mode', value: { string: 'hybrid' } }] });
});

it('keeps edited People search details visible when switching the feature off', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('vector.people.enabled', 'search', 'boolean', false), setting('vector.people.retention_posture', 'search', 'string', 'standard'));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'search' });
  const toggle = await screen.findByRole('switch', { name: 'Search people' });
  expect(screen.queryByLabelText('How long the provider keeps data')).toBeNull();
  await fireEvent.click(toggle);
  await fireEvent.input(screen.getByLabelText('How long the provider keeps data'), { target: { value: 'zero' } });
  await fireEvent.click(toggle);
  expect((screen.getByLabelText('How long the provider keeps data') as HTMLInputElement).value).toBe('zero');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [{ key: 'vector.people.retention_posture', value: { string: 'zero' } }] });
  await waitFor(() => expect(screen.queryByLabelText('How long the provider keeps data')).toBeNull());
});


it.each(['first enable', 'last disable', 'replacement'] as const)('saves valid provider transitions for %s and carries revisions', async transition => {
  const initiallyOn = transition !== 'first enable';
  const provider = { name: 'primary', kind: 'exa', enabled: initiallyOn, endpoint: 'https://provider.example.test', credential_id: 'people.enrichment.primary', credential: { configured: false, source: 'none' } };
  const doc = settingsDoc({ person_enrichment_providers: [provider, { ...provider, name: 'replacement', enabled: false }] });
  doc.settings.find(item => item.key === 'people.enrichment.enabled')!.value = typed(initiallyOn);
  let failPatch = transition === 'first enable';
  const { client, seen } = fixture(doc, request => request.method === 'PATCH' && failPatch ? Response.json({ message: 'Settings write failed' }, { status: 503 }) : undefined);
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.click(await screen.findByRole('switch', { name: 'Enable primary' }));
  if (transition === 'replacement') await fireEvent.click(screen.getByRole('switch', { name: 'Enable replacement' }));
  else await fireEvent.click(screen.getByRole('switch', { name: 'Look people up' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  if (failPatch) {
    await screen.findByText('Settings write failed');
    expect(screen.getByText('1 unsaved change')).toBeDefined();
    expect((doc.person_enrichment_providers[0] as typeof provider).enabled).toBe(true);
    failPatch = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  }
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  const writes = seen.filter(request => request.method !== 'GET' && request.path !== '/api/v1/search/coverage');
  expect(writes.map(request => [request.method, request.path.split('/').at(-1), request.ifMatch])).toEqual(transition === 'first enable'
    ? [['PUT', 'primary', '"s1"'], ['PATCH', 'settings', '"s2"'], ['PATCH', 'settings', '"s2"']]
    : transition === 'last disable' ? [['PATCH', 'settings', '"s1"'], ['PUT', 'primary', '"s2"']]
    : [['PUT', 'replacement', '"s1"'], ['PUT', 'primary', '"s2"']]);
  expect(doc.settings.find(item => item.key === 'people.enrichment.enabled')!.value).toEqual(typed(transition !== 'last disable'));
});


it('withholds a duplicate first build when coverage is unavailable', async () => {
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return Response.json({ lanes: [{ kind: 'message_embedding', configured: true, supported_actions: [] }] });
    if (request.url.includes('/embeddings/status')) return Response.json({ active_generation: null });
    if (request.url.includes('/search/coverage')) return Response.json({ eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1', status: 'initializing', actions: [] }, { status: 503 });
  });
  render(SettingsWorkspace, { client });
  await screen.findByText('On');
  expect(screen.queryByRole('button', { name: 'Build index, Message search' })).toBeNull();
});

it.each(['constructor', '__proto__'])('preserves and edits the saved endpoint for accepted provider name %s', async name => {
  const provider = { name, kind: 'exa', enabled: false, endpoint: 'https://provider.example.test', credential_id: `people.enrichment.${name}`, credential: { configured: false, source: 'none' } };
  const { client, seen } = fixture(settingsDoc({ person_enrichment_providers: [provider] }));
  render(SettingsWorkspace, { client, category: 'enrichment' });
  const endpoint = await screen.findByLabelText(`${name} endpoint`);
  expect((endpoint as HTMLInputElement).value).toBe('https://provider.example.test');
  await fireEvent.input(endpoint, { target: { value: 'https://replacement.example.test' } });
  await screen.findByRole('button', { name: 'Create profile' });
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PUT')?.body).toMatchObject({ endpoint: 'https://replacement.example.test' });
});
