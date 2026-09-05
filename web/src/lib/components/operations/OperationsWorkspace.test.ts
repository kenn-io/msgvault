import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { OperationsSnapshot, OperationsURLState } from '../../operations/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import OperationsWorkspace from './OperationsWorkspace.svelte';

const RUN_ONE = `op2.${'a'.repeat(32)}.syntheticRunOne`;
const RUN_TWO = `op2.${'b'.repeat(32)}.syntheticRunTwo`;

function urlState(overrides: Partial<OperationsURLState> = {}): OperationsURLState {
  return {
    operationLane: '',
    operationKind: '',
    operationState: '',
    operationStartedFrom: '',
    operationStartedBefore: '',
    operationRunID: null,
    operationStatus: '',
    ...overrides
  };
}

function run(overrides: Record<string, unknown> = {}) {
  return {
    id: RUN_ONE,
    kind: 'source_sync' as const,
    lane: 'messages' as const,
    trigger: 'manual' as const,
    state: 'succeeded' as const,
    started_at: '2026-08-30T10:00:00Z',
    finished_at: '2026-08-30T10:01:05Z',
    counters: [{ name: 'processed' as const, unit: 'messages' as const, value: 12 }],
    ...overrides
  };
}

function snapshot(overrides: Partial<OperationsSnapshot> = {}): OperationsSnapshot {
  const sourceRun = run();
  const messageEmbeddingRun = run({
    id: RUN_TWO, kind: 'message_embedding', state: 'running', finished_at: undefined
  });
  return {
    statusLanes: [
      { lane: 'messages', kinds: [
        {
          lane: 'messages', kind: 'source_sync', configured: true,
          history_availability: 'available', active: sourceRun, latest: sourceRun,
          latest_successful: sourceRun, related_status: 'listSourceStatus', supported_actions: []
        },
        {
          lane: 'messages', kind: 'message_embedding', configured: true,
          history_availability: 'unavailable', unavailable_code: 'history_adapter_unavailable',
          active: messageEmbeddingRun, latest: messageEmbeddingRun, supported_actions: []
        }
      ] },
      { lane: 'person_facts', kinds: [
        {
          lane: 'person_facts', kind: 'person_sweep', configured: false,
          history_availability: 'available', supported_actions: []
        },
        {
          lane: 'person_facts', kind: 'person_embedding', configured: true,
          history_availability: 'available', supported_actions: []
        },
        {
          lane: 'person_facts', kind: 'person_enrichment', configured: true,
          history_availability: 'available', supported_actions: []
        }
      ] },
      { lane: 'contacts', kinds: [{
        lane: 'contacts', kind: 'carddav_sync', configured: true,
        history_availability: 'available', related_status: 'getCardDAVStatus',
        supported_actions: ['carddav_sync']
      }] },
      { lane: 'documents', kinds: [
        {
          lane: 'documents', kind: 'document_extraction', configured: true,
          history_availability: 'available', related_status: 'getDocumentIndexStatus', supported_actions: []
        },
        {
          lane: 'documents', kind: 'document_embedding', configured: true,
          history_availability: 'available', related_status: 'getDocumentVectorStatus', supported_actions: []
        }
      ] },
      { lane: 'visual_attachments', kinds: [{
        lane: 'visual_attachments', kind: 'visual_embedding', configured: true,
        history_availability: 'available', related_status: 'getVisualAttachmentStatus',
        supported_actions: ['visual_resume']
      }] }
    ],
    rows: [sourceRun],
    unavailableKinds: [{
      lane: 'messages', kind: 'message_embedding', unavailable_code: 'history_adapter_unavailable'
    }],
    detail: null,
    membershipRevision: 7,
    nextCursor: null,
    statusReadable: true,
    historyReadable: true,
    initialLoading: false,
    backgroundLoading: false,
    paging: false,
    detailLoading: false,
    statusError: null,
    runsError: null,
    detailError: null,
    conflict: null,
    restartRequired: false,
    actionPending: null,
    actionConflict: null,
    actionError: null,
    ...overrides
  };
}

function controller(current: OperationsSnapshot = snapshot()) {
  return {
    snapshot: current,
    refresh: vi.fn(async () => undefined),
    loadMore: vi.fn(async () => undefined),
    restart: vi.fn(async () => undefined),
    runAction: vi.fn(async () => 'succeeded' as const)
  };
}

afterEach(() => vi.unstubAllGlobals());

describe('OperationsWorkspace', () => {
  it('renders exact public lanes, explicit per-kind availability and semantic run summaries', () => {
    const rendered = render(OperationsWorkspace, {
      controller: controller() as never,
      state: urlState()
    });

    const cards = screen.getByRole('region', { name: 'Operation lanes' });
    expect(within(cards).getAllByRole('heading', { level: 2 }).map((heading) => heading.textContent)).toEqual([
      'Messages', 'Facts', 'Contacts', 'Documents', 'Attachments'
    ]);
    expect(rendered.container.textContent).not.toContain('Person facts');
    expect(rendered.container.textContent).not.toContain('Visual attachments');
    expect(within(cards).getByText('Source sync')).toBeDefined();
    expect(within(cards).getByText('Message embedding')).toBeDefined();
    expect(within(cards).getByText('Person fact sweep')).toBeDefined();
    expect(within(cards).getByText('Not configured')).toBeDefined();
    expect(within(cards).getByText('History unavailable')).toBeDefined();
    expect(within(cards).getAllByText('Active').length).toBeGreaterThan(0);
    expect(within(cards).getAllByText('Latest').length).toBeGreaterThan(0);
    expect(within(cards).getAllByText('Last successful').length).toBeGreaterThan(0);
    expect(within(cards).getAllByText(/Running|Succeeded/).length).toBeGreaterThan(0);
  });

  it('claims no recorded runs only when history is available', () => {
    const current = snapshot();
    const statusLanes = current.statusLanes.map((lane) => ({
      ...lane,
      kinds: lane.kinds.map((kind) => kind.kind === 'message_embedding'
        ? { ...kind, active: undefined, latest: undefined, latest_successful: undefined }
        : kind)
    }));
    render(OperationsWorkspace, {
      controller: controller(snapshot({ statusLanes })) as never,
      state: urlState()
    });

    const unavailable = screen.getByRole('region', { name: 'Message embedding' });
    expect(within(unavailable).getByText('History unavailable')).toBeDefined();
    expect(within(unavailable).queryByText('No recorded runs')).toBeNull();

    const availableEmpty = screen.getByRole('region', { name: 'CardDAV sync' });
    expect(within(availableEmpty).getByText('History available')).toBeDefined();
    expect(within(availableEmpty).getByText('No recorded runs')).toBeDefined();
  });

  it('keeps advertised authority links and actions on lane status cards', async () => {
    const onNavigate = vi.fn();
    const actions = controller();
    render(OperationsWorkspace, {
      controller: actions as never,
      state: urlState(),
      onNavigate
    });

    const cards = screen.getByRole('region', { name: 'Operation lanes' });
    await fireEvent.click(within(cards).getByRole('button', { name: 'Open Sources status' }));
    expect(onNavigate).toHaveBeenCalledWith('listSourceStatus');
    await fireEvent.click(within(cards).getByRole('button', { name: 'Start CardDAV sync' }));
    expect(actions.runAction).toHaveBeenCalledWith('carddav_sync');
    expect(within(cards).getByRole('button', { name: 'Resume visual index' })).toBeDefined();
    expect(within(cards).queryByRole('button', { name: 'Build visual index' })).toBeNull();
  });

  it('passes the authoritative unconfigured document state to its related status panel', async () => {
    const onConfigure = vi.fn();
    const fetchFn = vi.fn<typeof fetch>();
    const statusLanes = snapshot().statusLanes.map((lane) => ({
      ...lane,
      kinds: lane.kinds.map((kind) => kind.kind === 'document_extraction'
        ? { ...kind, configured: false }
        : kind)
    }));
    render(OperationsWorkspace, {
      controller: controller(snapshot({ statusLanes })) as never,
      client: createAPIClient(fetchFn),
      state: urlState({ operationStatus: 'getDocumentIndexStatus' }),
      onConfigure
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Open document index settings' }));
    expect(onConfigure).toHaveBeenCalledWith('getDocumentIndexStatus');
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('emits canonical URL patches from filters and keeps available history visible when one kind degrades', async () => {
    const onStateChange = vi.fn();
    render(OperationsWorkspace, {
      controller: controller() as never,
      state: urlState({ operationRunID: RUN_ONE }),
      onStateChange
    });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Lane:/ }), 'Documents');
    expect(onStateChange).toHaveBeenLastCalledWith({
      operationLane: 'documents', operationRunID: null, operationStatus: ''
    });
    await chooseSelectOption(screen.getByRole('combobox', { name: /^State:/ }), 'Partial');
    expect(onStateChange).toHaveBeenLastCalledWith({
      operationState: 'partial', operationRunID: null, operationStatus: ''
    });

    expect(screen.getByRole('table', { name: 'Operation history' })).toBeDefined();
    expect(screen.getByRole('row', { name: /Source sync.*Manual.*Succeeded/ })).toBeDefined();
    expect(screen.getByRole('status', { name: 'Unavailable operation history' }).textContent)
      .toContain('Message embedding history is unavailable.');
  });

  it('shows newest-first bounded fields and commits an opaque row reference', async () => {
    const onStateChange = vi.fn();
    const older = run({
      id: RUN_ONE, started_at: '2026-08-30T09:00:00Z', finished_at: '2026-08-30T09:01:05Z'
    });
    const newer = run({
      id: RUN_TWO, kind: 'document_extraction', lane: 'documents', trigger: 'scheduled',
      state: 'partial', started_at: '2026-08-30T11:00:00Z', finished_at: '2026-08-30T11:00:02Z',
      counters: [{ name: 'failed', unit: 'writes', value: 2 }]
    });
    render(OperationsWorkspace, {
      controller: controller(snapshot({ rows: [older, newer], unavailableKinds: [] })) as never,
      state: urlState(),
      onStateChange
    });

    const table = screen.getByRole('table', { name: 'Operation history' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]!.textContent).toContain('Document extraction');
    expect(rows[0]!.textContent).toContain('2 seconds');
    expect(rows[0]!.textContent).toContain('2 failed writes');
    expect(rows[1]!.textContent).toContain('Source sync');
    expect(rows[1]!.textContent).toContain('1 minute 5 seconds');

    await fireEvent.click(within(rows[0]!).getByRole('button', { name: 'Open Document extraction run' }));
    expect(onStateChange).toHaveBeenCalledWith({ operationRunID: RUN_TWO });
  });

  it('renders only allowlisted detail, fixed error, related authority and advertised actions', async () => {
    const onNavigate = vi.fn();
    const current = snapshot({
      detail: {
        ...run({
          state: 'failed', error: { code: 'timeout', message: 'The operation timed out.' }
        }),
        related_status: 'listSourceStatus',
        supported_actions: ['carddav_sync']
      }
    });
    const actions = controller(current);
    render(OperationsWorkspace, {
      controller: actions as never,
      state: urlState({ operationRunID: RUN_ONE }),
      onNavigate
    });

    const detail = screen.getByRole('region', { name: 'Operation run detail' });
    expect(within(detail).getByText('timeout')).toBeDefined();
    expect(within(detail).getByText('The operation timed out.')).toBeDefined();
    expect(within(detail).getByRole('button', { name: 'Open Sources status' })).toBeDefined();
    expect(within(detail).getByRole('button', { name: 'Start CardDAV sync' })).toBeDefined();
    expect(within(detail).queryByRole('button', { name: 'Build visual index' })).toBeNull();
    expect(within(detail).queryByRole('button', { name: 'Resume visual index' })).toBeNull();

    await fireEvent.click(within(detail).getByRole('button', { name: 'Open Sources status' }));
    expect(onNavigate).toHaveBeenCalledWith('listSourceStatus');
    await fireEvent.click(within(detail).getByRole('button', { name: 'Start CardDAV sync' }));
    expect(actions.runAction).toHaveBeenCalledWith('carddav_sync');
  });

  it.each([
    ['succeeded', null, 'CardDAV sync request completed; current operation state was refreshed.'],
    ['failed', 'The operation started, but current state could not be refreshed.', 'The operation started, but current state could not be refreshed.'],
    ['discarded', null, null]
  ] as const)('announces only the explicit %s action outcome', async (outcome, actionError, announcement) => {
    const current = snapshot({
      detail: { ...run(), related_status: 'listSourceStatus', supported_actions: ['carddav_sync'] },
      actionError
    });
    const actions = {
      ...controller(current),
      runAction: vi.fn(async () => outcome)
    };
    const onAnnounce = vi.fn();
    render(OperationsWorkspace, {
      controller: actions as never,
      state: urlState({ operationRunID: RUN_ONE }),
      onAnnounce
    });

    await fireEvent.click(within(screen.getByRole('region', { name: 'Operation run detail' }))
      .getByRole('button', { name: 'Start CardDAV sync' }));

    if (announcement) expect(onAnnounce).toHaveBeenCalledWith(announcement);
    else expect(onAnnounce).not.toHaveBeenCalled();
  });

  it('restores focus to the invoking row when detail closes', async () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    const currentController = controller(snapshot({
      detail: { ...run(), related_status: 'listSourceStatus', supported_actions: [] }
    }));
    const rendered = render(OperationsWorkspace, {
      controller: currentController as never,
      state: urlState()
    });

    const invokingRow = screen.getByRole('button', { name: 'Open Source sync run' });
    await fireEvent.click(invokingRow);
    await rendered.rerender({
      controller: currentController as never,
      state: urlState({ operationRunID: RUN_ONE })
    });
    const restoredRow = screen.getByRole('button', { name: 'Open Source sync run' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close operation detail' }));
    await waitFor(() => expect(document.activeElement).toBe(restoredRow));
  });

  it('preserves selected detail and restores exact duplicate focus when opaque references rotate', async () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    const oldFirst = run({ id: `${RUN_ONE}-first` });
    const oldSecond = run({ id: `${RUN_ONE}-second` });
    const newFirst = run({ id: `${RUN_TWO}-first` });
    const newSecond = run({ id: `${RUN_TWO}-second` });
    const onStateChange = vi.fn();
    const rendered = render(OperationsWorkspace, {
      controller: controller(snapshot({ rows: [oldFirst, oldSecond] })) as never,
      state: urlState(),
      onStateChange
    });
    const oldButtons = screen.getAllByRole('button', { name: 'Open Source sync run' });
    await fireEvent.click(oldButtons[1]!);
    onStateChange.mockClear();

    await rendered.rerender({
      controller: controller(snapshot({
        rows: [newFirst, newSecond],
        detail: { ...oldSecond, related_status: 'listSourceStatus', supported_actions: [] }
      })) as never,
      state: urlState({ operationRunID: `${RUN_ONE}-second` }),
      onStateChange
    });
    expect(onStateChange).not.toHaveBeenCalled();
    const newButtons = screen.getAllByRole('button', { name: 'Open Source sync run' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close operation detail' }));
    expect(onStateChange).toHaveBeenCalledExactlyOnceWith({ operationRunID: null });
    await waitFor(() => expect(document.activeElement).toBe(newButtons[1]));
    expect(document.activeElement).not.toBe(newButtons[0]);
  });

  it('keeps detail identity when its same-timestamp row disappears but restores nearby focus', async () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    const selected = run({ id: `${RUN_ONE}-selected` });
    const oldPeer = run({ id: `${RUN_ONE}-peer` });
    const newPeer = run({ id: `${RUN_TWO}-peer` });
    const onStateChange = vi.fn();
    const rendered = render(OperationsWorkspace, {
      controller: controller(snapshot({ rows: [selected, oldPeer] })) as never,
      state: urlState(),
      onStateChange
    });
    await fireEvent.click(screen.getAllByRole('button', { name: 'Open Source sync run' })[0]!);
    onStateChange.mockClear();

    await rendered.rerender({
      controller: controller(snapshot({
        rows: [newPeer],
        detail: { ...selected, related_status: 'listSourceStatus', supported_actions: [] }
      })) as never,
      state: urlState({ operationRunID: selected.id }),
      onStateChange
    });

    expect(onStateChange).not.toHaveBeenCalled();
    const fallbackRow = screen.getByRole('button', { name: 'Open Source sync run' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close operation detail' }));
    expect(onStateChange).toHaveBeenCalledExactlyOnceWith({ operationRunID: null });
    await waitFor(() => expect(document.activeElement).toBe(fallbackRow));
  });

  it('closes detail with Escape and uses focused content on narrow screens', async () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    const onStateChange = vi.fn();
    const selected = snapshot({
      detail: { ...run(), related_status: 'listSourceStatus', supported_actions: [] }
    });
    render(OperationsWorkspace, {
      controller: controller(selected) as never,
      state: urlState({ operationRunID: RUN_ONE }),
      onStateChange
    });

    const focused = screen.getByRole('region', { name: 'Operation detail focused content' });
    expect(focused).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Operation lanes' })).toBeNull();
    await fireEvent.keyDown(window, { key: 'Escape' });
    expect(onStateChange).toHaveBeenCalledWith({ operationRunID: null });
  });

  it.each([
    ['detail loading', { detailLoading: true }, 'status', 'Operation detail loading'],
    ['detail failure', { detailError: 'Unable to load operation detail.' }, 'alert', 'Operation detail failure'],
    ['detail conflict', {
      conflict: 'Operation history changed. Restart from the first page.', restartRequired: true
    }, 'alert', 'Operation history conflict'],
    ['action progress', { actionPending: 'visual_resume' as const }, 'status', 'Operation action progress'],
    ['action conflict', { actionConflict: 'The operation state changed.' }, 'alert', 'Operation action conflict'],
    ['action failure', { actionError: 'Unable to start the operation.' }, 'alert', 'Operation action failure']
  ] as const)('keeps narrow recovery controls and semantic %s state visible', (_case, overrides, role, name) => {
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    render(OperationsWorkspace, {
      controller: controller(snapshot({ detail: null, ...overrides })) as never,
      state: urlState({ operationRunID: RUN_ONE })
    });

    const focused = screen.getByRole('region', { name: 'Operation detail focused content' });
    expect(within(focused).getByRole('button', { name: 'Back to operation history' })).toBeDefined();
    expect(within(focused).getByRole(role, { name })).toBeDefined();
    if (name === 'Operation history conflict') {
      expect(within(focused).getByRole('button', { name: 'Restart operation history' })).toBeDefined();
    }
  });

  it('shows loading, failed, conflicted, degraded and empty history states exclusively', async () => {
    const unreadableLanes = snapshot().statusLanes.map((lane) => ({ ...lane, kinds: [] }));
    const rendered = render(OperationsWorkspace, {
      controller: controller(snapshot({
        statusLanes: unreadableLanes,
        rows: [],
        statusReadable: false,
        historyReadable: false,
        initialLoading: true,
        unavailableKinds: []
      })) as never,
      state: urlState()
    });

    expect(screen.getByRole('status', { name: 'Operations loading' })).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Operation lanes' })).toBeNull();
    expect(screen.queryByRole('status', { name: 'Operation history state' })).toBeNull();

    await rendered.rerender({
      controller: controller(snapshot({
        statusLanes: unreadableLanes,
        rows: [],
        statusReadable: false,
        historyReadable: false,
        runsError: 'Unable to load operation history.',
        unavailableKinds: []
      })) as never,
      state: urlState()
    });
    expect(screen.getByRole('alert', { name: 'Operation history failure' })).toBeDefined();
    expect(screen.queryByRole('status', { name: 'Operation history state' })).toBeNull();

    await rendered.rerender({
      controller: controller(snapshot({
        rows: [], historyReadable: false,
        conflict: 'Operation history changed. Restart from the first page.', restartRequired: true,
        unavailableKinds: []
      })) as never,
      state: urlState()
    });
    expect(screen.getByRole('alert', { name: 'Operation history conflict' })).toBeDefined();
    expect(screen.queryByRole('status', { name: 'Operation history state' })).toBeNull();

    await rendered.rerender({
      controller: controller(snapshot({ rows: [], historyReadable: true })) as never,
      state: urlState()
    });
    expect(screen.getByRole('status', { name: 'Unavailable operation history' })).toBeDefined();
    expect(screen.queryByRole('status', { name: 'Operation history state' })).toBeNull();

    await rendered.rerender({
      controller: controller(snapshot({ rows: [], historyReadable: true, unavailableKinds: [] })) as never,
      state: urlState()
    });
    expect(screen.getByRole('status', { name: 'Operation history state' })).toBeDefined();
  });

  it('uses status and alert roles for loading, refresh, empty, conflict and action failures', () => {
    const current = snapshot({
      rows: [], initialLoading: true, backgroundLoading: true,
      conflict: 'Operation history changed. Restart from the first page.', restartRequired: true,
      actionPending: 'visual_resume', actionConflict: 'The operation state changed.',
      actionError: 'Unable to start the operation.'
    });
    const rendered = render(OperationsWorkspace, {
      controller: controller(current) as never,
      state: urlState()
    });

    expect(screen.getByRole('status', { name: 'Operations loading' })).toBeDefined();
    expect(screen.getByRole('status', { name: 'Operations refresh' })).toBeDefined();
    expect(screen.getByRole('alert', { name: 'Operation history conflict' })).toBeDefined();
    expect(screen.getByRole('status', { name: 'Operation action progress' })).toBeDefined();
    expect(screen.getByRole('alert', { name: 'Operation action conflict' })).toBeDefined();
    expect(screen.getByRole('alert', { name: 'Operation action failure' })).toBeDefined();

    rendered.unmount();
    render(OperationsWorkspace, {
      controller: controller(snapshot({ rows: [], unavailableKinds: [] })) as never,
      state: urlState()
    });
    expect(screen.getByRole('status', { name: 'Operation history state' })).toBeDefined();
  });
});
