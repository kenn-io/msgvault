import { expect, type Page, type Request, type Route } from '@playwright/test';
import type { components } from '../../../src/lib/api/generated/schema';

const FIXTURE_TIME = '2026-08-28T12:00:00Z';
const SYNTHETIC_PASSWORD = 'synthetic-carddav-password';

export const CARD_DAV_FORBIDDEN_MARKERS = [
  'https://forbidden-carddav.example.test/private/',
  '/forbidden-carddav-href/contacts.vcf',
  'BEGIN:VCARD\nFN:Forbidden Fixture Card\nEND:VCARD',
  'forbidden-etag-marker',
  'forbidden-uid-marker',
  'forbidden-header-marker',
  'forbidden-credential-marker'
] as const;

export async function assertCardDAVForbiddenMarkersAbsent(page: Page): Promise<void> {
  const surface = [
    await page.locator('body').innerText(),
    await page.locator('body').ariaSnapshot(),
    await page.locator('[title]').evaluateAll((elements) =>
      elements.map((element) => element.getAttribute('title') ?? '').join('\n')),
    await page.locator('[role="alert"], [role="status"]').allTextContents()
  ].flat().join('\n');
  for (const marker of CARD_DAV_FORBIDDEN_MARKERS) expect(surface).not.toContain(marker);
}

type AccountRequest = components['schemas']['CardDAVAccountRequest'];
type Book = components['schemas']['CardDAVBookResponse'];
type BookRoles = components['schemas']['CardDAVBookRolesRequest'];
type Conflict = components['schemas']['CardDAVConflictResponse'];
type ConflictDetail = components['schemas']['CardDAVConflictDetailResponse'];
type Publication = components['schemas']['CardDAVPublicationResponse'];
type Run = components['schemas']['CardDAVRunResponse'];

export type CardDAVCapturedRequest = {
  method: string;
  path: string;
  query: Record<string, string>;
  body?: unknown;
  passwordMatched?: boolean;
};

export type CardDAVFixtureOptions = {
  configured?: boolean;
  staleConflictOnce?: boolean;
};

export type CardDAVFixture = {
  requests: CardDAVCapturedRequest[];
  password: string;
  failNextPublicationRead(): void;
  holdNextConflictResolution(): () => void;
};

export async function installCardDAV(
  page: Page,
  options: CardDAVFixtureOptions = {}
): Promise<CardDAVFixture> {
  const requests: CardDAVCapturedRequest[] = [];
  let accountPhase: 'unconfigured' | 'tested' | 'configured' = options.configured ? 'configured' : 'unconfigured';
  let rolePhase: 'original' | 'refreshed' | 'committed' = 'original';
  let staleRoleOnce = true;
  let syncPhase: 'idle' | 'running' | 'terminal' = 'idle';
  let runningStatusReads = 0;
  let conflictPhase: 'unresolved' | 'refreshed' | 'resolved' = 'unresolved';
  let staleConflictOnce = options.staleConflictOnce ?? true;
  let publicationPhase: 'unpublished' | 'published' | 'conflict' = 'unpublished';
  let failPublicationRead = false;
  let heldResolution: Promise<void> | undefined;
  let releaseHeldResolution: (() => void) | undefined;

  await page.route('**/api/session', (route) => route.fulfill({
    json: { auth_mode: 'loopback', https: false, plain_http_warning: false }
  }));
  await page.route('**/api/v1/settings', (route) => route.fulfill({
    headers: { ETag: '"settings-carddav-1"' },
    json: settingsDocument(accountPhase === 'configured')
  }));
  await page.route('**/api/v1/carddav/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const method = request.method();

    if (url.pathname === '/api/v1/carddav/account/test' && method === 'POST') {
      const body = accountBody(request);
      const passwordMatched = body.password === SYNTHETIC_PASSWORD;
      expect(passwordMatched).toBe(true);
      requests.push({ ...capture(method, url, safeBody(request)), passwordMatched });
      accountPhase = 'tested';
      return route.fulfill({ json: accountResponse(body) });
    }

    if (url.pathname === '/api/v1/carddav/account' && method === 'PUT') {
      const body = accountBody(request);
      const passwordMatched = body.password === SYNTHETIC_PASSWORD;
      expect(passwordMatched).toBe(true);
      requests.push({ ...capture(method, url, safeBody(request)), passwordMatched });
      accountPhase = 'configured';
      return route.fulfill({ json: accountResponse(body) });
    }

    if (method === 'GET') {
      requests.push(capture(method, url));
      if (url.pathname === '/api/v1/carddav/status') {
        if (accountPhase !== 'configured') return route.fulfill({ json: unavailableStatus() });
        if (syncPhase === 'running') {
          runningStatusReads += 1;
          if (runningStatusReads >= 3) syncPhase = 'terminal';
        }
        return route.fulfill({ json: currentStatus(syncPhase, runningStatusReads) });
      }
      if (url.pathname === '/api/v1/carddav/books') {
        return route.fulfill({ json: { books: booksFor(rolePhase) } });
      }
      if (url.pathname === '/api/v1/carddav/runs') {
        const before = url.searchParams.get('before_id');
        return route.fulfill({ json: before
          ? { runs: [historyRun(80)], next_before_id: undefined }
          : { runs: syncPhase === 'terminal' ? [historyRun(102), historyRun(91)] : [historyRun(91)], next_before_id: 90 }
        });
      }
      if (url.pathname === '/api/v1/carddav/conflicts') {
        if (accountPhase !== 'configured') return unavailable(route);
        return route.fulfill({ json: { conflicts: conflictPhase === 'resolved' ? [] : [conflictListItem()] } });
      }
      if (url.pathname === '/api/v1/carddav/conflicts/41') {
        if (accountPhase !== 'configured') return unavailable(route);
        return route.fulfill({ json: conflictDetail(41, conflictPhase === 'resolved' ? 'resolved' : 'unresolved') });
      }
      if (url.pathname === '/api/v1/carddav/conflicts/42') {
        if (accountPhase !== 'configured') return unavailable(route);
        return route.fulfill({ json: conflictDetail(42, 'resolved') });
      }
      if (url.pathname === '/api/v1/carddav/publications/42') {
        if (accountPhase !== 'configured') return unavailable(route);
        if (failPublicationRead) {
          failPublicationRead = false;
          return route.fulfill({ status: 503, json: { error: 'carddav_read_failed', message: 'Synthetic publication read failed.' } });
        }
        return route.fulfill({ json: publication(publicationPhase) });
      }
    }

    if (url.pathname === '/api/v1/carddav/books/6' && method === 'PATCH') {
      const body = request.postDataJSON() as BookRoles;
      requests.push(capture(method, url, body));
      if (staleRoleOnce) {
        staleRoleOnce = false;
        rolePhase = 'refreshed';
        return route.fulfill({ status: 409, json: { error: 'carddav_book_stale', message: 'Synthetic roles changed.' } });
      }
      rolePhase = 'committed';
      return route.fulfill({ json: booksFor(rolePhase)[1] });
    }

    if (url.pathname === '/api/v1/carddav/sync' && method === 'POST') {
      const body = request.postDataJSON() as components['schemas']['CardDAVSyncRequest'];
      requests.push(capture(method, url, body));
      syncPhase = 'running';
      runningStatusReads = 0;
      return route.fulfill({ json: activeRun(102, 1) });
    }

    if (url.pathname === '/api/v1/carddav/conflicts/41/resolve' && method === 'POST') {
      const body = request.postDataJSON() as components['schemas']['CardDAVResolveRequest'];
      requests.push(capture(method, url, body));
      if (staleConflictOnce) {
        staleConflictOnce = false;
        conflictPhase = 'refreshed';
        return route.fulfill({ status: 409, json: { error: 'carddav_conflict_stale', message: 'Synthetic conflict changed.' } });
      }
      if (heldResolution) await heldResolution;
      heldResolution = undefined;
      releaseHeldResolution = undefined;
      conflictPhase = 'resolved';
      return route.fulfill({ json: { id: 41, status: 'resolved', resolution: body.choice } });
    }

    if (url.pathname === '/api/v1/carddav/publications/42' && method === 'POST') {
      requests.push(capture(method, url));
      publicationPhase = 'published';
      return route.fulfill({ json: publication(publicationPhase) });
    }

    if (url.pathname === '/api/v1/carddav/publications/42' && method === 'DELETE') {
      requests.push(capture(method, url));
      publicationPhase = 'conflict';
      return route.fulfill({ status: 503, json: { error: 'carddav_write_failed', message: 'Synthetic ambiguous publication result.' } });
    }

    requests.push(capture(method, url, safeBody(request)));
    return route.fulfill({ status: 404, json: { error: 'fixture_route_missing', message: 'Synthetic route is not modeled.' } });
  });

  return {
    requests,
    password: SYNTHETIC_PASSWORD,
    failNextPublicationRead() {
      failPublicationRead = true;
    },
    holdNextConflictResolution() {
      if (heldResolution) throw new Error('A synthetic conflict resolution is already held.');
      heldResolution = new Promise<void>((resolve) => { releaseHeldResolution = resolve; });
      return () => releaseHeldResolution?.();
    }
  };
}

function accountBody(request: Request): AccountRequest {
  return request.postDataJSON() as AccountRequest;
}

function accountResponse(body: AccountRequest): components['schemas']['CardDAVAccountResponse'] {
  return {
    base_url: body.base_url,
    username: body.username,
    enabled: body.enabled,
    schedule: body.schedule,
    books: 2
  };
}

function safeBody(request: Request): unknown {
  if (!request.postData()) return undefined;
  const body: unknown = request.postDataJSON();
  const path = new URL(request.url()).pathname;
  if (path === '/api/v1/carddav/account' || path.startsWith('/api/v1/carddav/account/')) {
    return safeUnknownAccountBody(body);
  }
  return body;
}

function safeUnknownAccountBody(body: unknown): Record<string, string | boolean> {
  if (!body || typeof body !== 'object' || Array.isArray(body)) return {};
  const source = body as Record<string, unknown>;
  const safe: Record<string, string | boolean> = {};
  if (typeof source.base_url === 'string') safe.base_url = source.base_url;
  if (typeof source.username === 'string') safe.username = source.username;
  if (typeof source.enabled === 'boolean') safe.enabled = source.enabled;
  if (typeof source.schedule === 'string') safe.schedule = source.schedule;
  return safe;
}

function capture(method: string, url: URL, body?: unknown): CardDAVCapturedRequest {
  return {
    method,
    path: url.pathname,
    query: Object.fromEntries(url.searchParams),
    ...(body === undefined ? {} : { body })
  };
}

function settingsDocument(configured: boolean): components['schemas']['SettingsResponse'] {
  return {
    settings: [
      setting('web.theme', 'browser', 'string', { string: 'light' }),
      setting('web.density', 'browser', 'string', { string: 'compact' }),
      setting('carddav.base_url', 'integrations', 'string', { string: configured ? 'https://carddav.example.test/' : '' }),
      setting('carddav.username', 'integrations', 'string', { string: configured ? 'synthetic-user' : '' }),
      {
        key: 'carddav.password', group: 'integrations', kind: 'secret', restart_required: false,
        secret: { configured }
      },
      setting('carddav.enabled', 'integrations', 'boolean', { boolean: configured }),
      setting('carddav.schedule', 'integrations', 'string', { string: configured ? '0 2 * * *' : '' })
    ],
    pending_restart: false
  };
}

function setting(
  key: string,
  group: components['schemas']['Setting']['group'],
  kind: components['schemas']['Setting']['kind'],
  value: components['schemas']['SettingValue']
): components['schemas']['Setting'] {
  return { key, group, kind, value, restart_required: false };
}

function unavailableStatus(): components['schemas']['CardDAVStatusResponse'] {
  return {
    configured: false,
    available: false,
    credential_configured: false,
    enabled: false,
    scheduled: false,
    schedule: '',
    repair_reason: 'account_missing'
  };
}

function currentStatus(
  phase: 'idle' | 'running' | 'terminal',
  runningStatusReads: number
): components['schemas']['CardDAVStatusResponse'] {
  const latest = phase === 'terminal' ? historyRun(102) : historyRun(91);
  return {
    configured: true,
    available: true,
    credential_configured: true,
    enabled: true,
    scheduled: true,
    schedule: '0 2 * * *',
    next_scheduled_at: '2026-08-29T02:00:00Z',
    account: {
      base_url: CARD_DAV_FORBIDDEN_MARKERS[0],
      username: 'synthetic-user',
      private_header: CARD_DAV_FORBIDDEN_MARKERS[5]
    },
    ...(phase === 'running'
      ? { active: activeRun(102, Math.max(1, runningStatusReads)), latest: historyRun(91), latest_successful: historyRun(91) }
      : {}),
    ...(phase !== 'running' ? { latest, latest_successful: latest } : {})
  };
}

function activeRun(id: number, updated: number): Run {
  return {
    id,
    state: 'running',
    trigger: 'manual',
    full: false,
    started_at: FIXTURE_TIME,
    books: 2,
    created: 1,
    updated,
    removed: 0,
    private_credential: CARD_DAV_FORBIDDEN_MARKERS[6]
  };
}

function historyRun(id: number): Run {
  const day = id === 102 ? '28' : id === 91 ? '27' : '26';
  return {
    id,
    state: 'succeeded',
    trigger: 'manual',
    full: id === 80,
    started_at: `2026-08-${day}T12:00:00Z`,
    finished_at: `2026-08-${day}T12:05:00Z`,
    books: 2,
    created: id === 102 ? 1 : 0,
    updated: id === 102 ? 2 : 1,
    removed: 0,
    private_uid: CARD_DAV_FORBIDDEN_MARKERS[4]
  };
}

function booksFor(phase: 'original' | 'refreshed' | 'committed'): Book[] {
  const secondIsTarget = phase === 'committed';
  return [
    {
      id: 5,
      name: phase === 'refreshed' ? 'Synthetic contacts (refreshed)' : 'Synthetic contacts',
      url: CARD_DAV_FORBIDDEN_MARKERS[0],
      subscribed: true,
      lookup_source: true,
      write_target: !secondIsTarget,
      needs_full_reconcile: phase === 'refreshed'
    },
    {
      id: 6,
      name: 'Synthetic publishing',
      url: `${CARD_DAV_FORBIDDEN_MARKERS[0]}publishing/`,
      subscribed: secondIsTarget,
      lookup_source: false,
      write_target: secondIsTarget,
      needs_full_reconcile: false
    }
  ];
}

function conflictListItem(): Conflict {
  return {
    id: 41,
    address_book: { id: 5, name: 'Synthetic contacts' },
    status: 'unresolved',
    local_state: 'present',
    remote_state: 'deleted',
    allowed_resolutions: ['keep_local', 'keep_remote'],
    updated_at: FIXTURE_TIME,
    href: CARD_DAV_FORBIDDEN_MARKERS[1],
    etag: CARD_DAV_FORBIDDEN_MARKERS[3]
  };
}

function conflictDetail(id: number, status: 'unresolved' | 'resolved'): ConflictDetail {
  return {
    id,
    address_book: { id: 5, name: 'Synthetic contacts' },
    status,
    base: { state: 'unavailable', emails: [], phones: [] },
    local: {
      state: 'present',
      display_name: 'Synthetic Local',
      emails: ['local@example.com'],
      phones: ['+1 555 0100'],
      truncated: true,
      local_vcard: CARD_DAV_FORBIDDEN_MARKERS[2]
    },
    remote: { state: 'deleted', emails: [], phones: [], href: CARD_DAV_FORBIDDEN_MARKERS[1] },
    allowed_resolutions: status === 'resolved' ? [] : ['keep_local', 'keep_remote'],
    created_at: '2026-08-27T12:00:00Z',
    updated_at: FIXTURE_TIME,
    ...(status === 'resolved' ? { resolution: 'keep_local' as const, resolved_at: FIXTURE_TIME } : {}),
    uid: CARD_DAV_FORBIDDEN_MARKERS[4],
    request_headers: { authorization: CARD_DAV_FORBIDDEN_MARKERS[5] }
  };
}

function publication(phase: 'unpublished' | 'published' | 'conflict'): Publication {
  return {
    person_id: 42,
    state: phase,
    desired: phase !== 'unpublished',
    address_book: { id: 5, name: 'Synthetic contacts' },
    ...(phase === 'conflict' ? { conflict_id: 42 } : {}),
    href: CARD_DAV_FORBIDDEN_MARKERS[1],
    raw_vcard: CARD_DAV_FORBIDDEN_MARKERS[2],
    etag: CARD_DAV_FORBIDDEN_MARKERS[3],
    credential: CARD_DAV_FORBIDDEN_MARKERS[6]
  };
}

function unavailable(route: Route) {
  return route.fulfill({
    status: 503,
    json: { error: 'carddav_unavailable', message: 'Synthetic optional service is not configured.' }
  });
}
