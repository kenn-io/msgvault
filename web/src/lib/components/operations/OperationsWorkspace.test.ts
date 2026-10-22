import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { OPERATION_KIND_LABELS } from '../../operations/labels';
import type {
  OperationKind,
  OperationLane,
  OperationsSnapshot,
  OperationsURLState
} from '../../operations/models';
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
    statusUpdatedAt: null,
    statusRefreshing: false,
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
    refreshStatus: vi.fn(async () => true),
    loadMore: vi.fn(async () => undefined),
    restart: vi.fn(async () => undefined),
    runAction: vi.fn(async () => 'succeeded' as const)
  };
}

const off = (kind: OperationKind, lane: OperationLane, related?: string) => ({
  lane, kind, configured: false, history_availability: 'available' as const, supported_actions: [],
  ...(related ? { related_status: related } : {})
});

function renderKinds(kinds: Array<ReturnType<typeof off>>, props: Record<string, unknown> = {}) {
  const lanes = ['messages', 'person_facts', 'contacts', 'visual_attachments'] as const;
  const statusLanes = lanes.map((lane) => ({ lane, kinds: kinds.filter((kind) => kind.lane === lane) }));
  return render(OperationsWorkspace, {
    controller: controller(snapshot({ statusLanes: statusLanes as never })) as never,
    state: urlState(),
    ...props
  });
}

afterEach(() => vi.unstubAllGlobals());

describe('OperationsWorkspace', () => {
  it('lists every lane with one row per kind and no History available text', () => {
    render(OperationsWorkspace, { controller: controller() as never, state: urlState() });
    const region = screen.getByRole('region', { name: 'Operation lanes' });
    expect(within(region).getAllByRole('heading', { level: 2 }).map((heading) => heading.textContent))
      .toEqual(['Messages', 'Person facts', 'Contacts', 'Attachments']);
    expect(within(screen.getByRole('list', { name: 'Messages' })).getAllByRole('listitem')).toHaveLength(2);
    const embedding = screen.getByRole('listitem', { name: 'Message embedding' });
    expect(within(embedding).getByText('Running')).toBeDefined();
    expect(within(embedding).getByText('History unavailable')).toBeDefined();
    expect(region.textContent).not.toContain('History available');
    expect(within(screen.getByRole('listitem', { name: 'Person fact sweep' })).getByText('Off')).toBeDefined();
    expect(within(screen.getByRole('listitem', { name: 'Person embedding' })).getByText('No runs yet')).toBeDefined();
  });

  it('leaves focus where the person moved it while a status refresh was pending', async () => {
    let finish!: (value: boolean) => void;
    const stub = controller();
    stub.refreshStatus.mockImplementation(() => new Promise<boolean>((resolve) => { finish = resolve; }));
    render(OperationsWorkspace, { controller: stub as never, state: urlState() });
    const refresh = screen.getByRole('button', { name: 'Refresh operation status' });
    refresh.focus();
    await fireEvent.click(refresh);
    expect(stub.refreshStatus).toHaveBeenCalledOnce();

    const reload = screen.getByRole('button', { name: 'Reload run history' });
    reload.focus();
    finish(true);

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(document.activeElement).toBe(reload);
  });

  it('shows Status unavailable for a lane without kinds', () => {
    renderKinds([]);
    expect(screen.getAllByText('Status unavailable')).toHaveLength(4);
  });

  it.each([
    ['message_embedding', 'messages', { settingsCategory: 'search', settingsAuthority: 'semantic_search' }],
    ['person_embedding', 'person_facts', { settingsCategory: 'search', settingsAuthority: 'person_embeddings' }],
    ['visual_embedding', 'visual_attachments', { settingsCategory: 'search', settingsAuthority: 'visual_attachments' }],
    ['person_enrichment', 'person_facts', { settingsCategory: 'enrichment', settingsAuthority: '' }],
    ['person_sweep', 'person_facts', { settingsCategory: 'people', settingsAuthority: '' }]
  ] as const)('sends an Off %s row to Settings', async (kind, lane, target) => {
    const onSetUp = vi.fn();
    renderKinds([off(kind, lane)], { onSetUp });
    const row = screen.getByRole('listitem', { name: OPERATION_KIND_LABELS[kind] });
    expect(within(row).getByText('Off')).toBeDefined();
    await fireEvent.click(within(row).getByRole('button', { name: `Set up ${OPERATION_KIND_LABELS[kind]}` }));
    expect(onSetUp).toHaveBeenCalledWith(target);
  });



  it('keeps the related-status button and no Set up for Off CardDAV and source rows', () => {
    renderKinds([
      off('carddav_sync', 'contacts', 'getCardDAVStatus'),
      off('source_sync', 'messages', 'listSourceStatus')
    ]);
    expect(screen.getByRole('button', { name: 'Open CardDAV settings' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open Sources status' })).toBeDefined();
    expect(screen.queryByRole('button', { name: /^Set up/ })).toBeNull();
  });

  it('claims no runs only when history is available', () => {
    renderKinds([
      { ...off('message_embedding', 'messages'), configured: true, history_availability: 'unavailable' } as never,
      { ...off('carddav_sync', 'contacts'), configured: true }
    ]);
    const unavailable = screen.getByRole('listitem', { name: 'Message embedding' });
    expect(within(unavailable).getByText('History unavailable')).toBeDefined();
    expect(within(unavailable).queryByText('No runs yet')).toBeNull();
    expect(within(screen.getByRole('listitem', { name: 'CardDAV sync' })).getByText('No runs yet')).toBeDefined();
  });

  it('shows an active run instead of Off for a kind that is not configured', () => {
    const active = {
      id: 'x', kind: 'message_embedding' as const, lane: 'messages' as const, state: 'queued' as const,
      started_at: '2026-08-30T12:00:00Z', counters: []
    };
    renderKinds([{ ...off('message_embedding', 'messages'), active } as never]);
    const row = screen.getByRole('listitem', { name: 'Message embedding' });
    expect(within(row).getByText('Queued')).toBeDefined();
    expect(within(row).queryByText('Off')).toBeNull();
  });

  it('shows when the last success differs from the latest run', () => {
    const partial = {
      id: 'x', kind: 'source_sync' as const, lane: 'messages' as const, state: 'partial' as const,
      started_at: '2026-08-30T12:00:00Z', counters: []
    };
    const success = { ...partial, id: 'y', state: 'succeeded' as const, started_at: '2026-08-29T12:00:00Z' };
    renderKinds([{
      ...off('source_sync', 'messages'), configured: true, latest: partial, latest_successful: success
    } as never]);
    const row = screen.getByRole('listitem', { name: 'Source sync' });
    expect(within(row).getByText('Partial')).toBeDefined();
    expect(within(row).getByText(/Last succeeded/)).toBeDefined();
  });

  it('omits Last succeeded when the latest run succeeded', () => {
    render(OperationsWorkspace, { controller: controller() as never, state: urlState() });
    const row = screen.getByRole('listitem', { name: 'Source sync' });
    expect(within(row).getByText('Succeeded')).toBeDefined();
    expect(within(row).queryByText(/Last succeeded/)).toBeNull();
  });

  it('keeps advertised authority links and actions on lane status rows', async () => {
    const onNavigate = vi.fn();
    const actions = controller();
    render(OperationsWorkspace, {
      controller: actions as never,
      state: urlState(),
      onNavigate
    });

    const lanes = screen.getByRole('region', { name: 'Operation lanes' });
    await fireEvent.click(within(lanes).getByRole('button', { name: 'Open Sources status' }));
    expect(onNavigate).toHaveBeenCalledWith('listSourceStatus');
    await fireEvent.click(within(lanes).getByRole('button', { name: 'Start CardDAV sync' }));
    expect(actions.runAction).toHaveBeenCalledWith('carddav_sync');
    expect(within(lanes).getByRole('button', { name: 'Resume visual index' })).toBeDefined();
    expect(within(lanes).queryByRole('button', { name: 'Build visual index' })).toBeNull();
  });



  it('emits canonical URL patches from filters and keeps available history visible when one kind degrades', async () => {
    const onStateChange = vi.fn();
    render(OperationsWorkspace, {
      controller: controller() as never,
      state: urlState({ operationRunID: RUN_ONE }),
      onStateChange
    });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Lane:/ }), 'Attachments');
    expect(onStateChange).toHaveBeenLastCalledWith({
      operationLane: 'visual_attachments', operationRunID: null, operationStatus: ''
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
      id: RUN_TWO, kind: 'visual_embedding', lane: 'visual_attachments', trigger: 'scheduled',
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
    expect(rows[0]!.textContent).toContain('Visual embedding');
    expect(rows[0]!.textContent).toContain('2 seconds');
    expect(rows[0]!.textContent).toContain('2 writes failed');
    expect(rows[1]!.textContent).toContain('Source sync');
    expect(rows[1]!.textContent).toContain('1 minute 5 seconds');

    await fireEvent.click(within(rows[0]!).getByRole('button', { name: 'Open Visual embedding run' }));
    expect(onStateChange).toHaveBeenCalledWith({ operationRunID: RUN_TWO });
  });

  it('shows triggers, counters, and failure sentences in the runs table', () => {
    const failed = run({ id: RUN_TWO, state: 'failed', trigger: undefined,
      counters: [
        { name: 'processed', unit: 'messages', value: 20 }, { name: 'added', unit: 'messages', value: 20 },
        { name: 'updated', unit: 'messages', value: 0 }, { name: 'item_errors', unit: 'messages', value: 0 }
      ],
      error: { code: 'source_sync_failed', message: 'Source sync failed.' } });
    render(OperationsWorkspace, {
      controller: controller(snapshot({ rows: [failed], unavailableKinds: [] })) as never,
      state: urlState()
    });
    const row = within(screen.getByRole('table', { name: 'Operation history' })).getAllByRole('row')[1]!;
    expect(row.textContent).toContain('—');
    expect(row.textContent).toContain('20 messages processed · 20 added');
    expect(row.textContent).not.toContain('item errors');
    expect(row.textContent).toContain('Failed');
    expect(row.textContent).toContain('Source sync failed.');
  });

  it('leads the run detail error with the server sentence and keeps every counter', () => {
    const detail = { ...run({ state: 'failed', counters: [{ name: 'item_errors', unit: 'messages', value: 0 }],
      error: { code: 'source_sync_failed', message: 'Source sync failed.' } }), supported_actions: [] };
    render(OperationsWorkspace, {
      controller: controller(snapshot({ detail })) as never,
      state: urlState({ operationRunID: RUN_ONE })
    });
    const error = screen.getByRole('alert', { name: 'Operation error' });
    expect(error.firstElementChild?.textContent).toBe('Source sync failed.');
    expect(within(error).getByText('Code: source_sync_failed').tagName).toBe('CODE');
    expect(screen.getByText('Item errors')).toBeDefined();
    expect(screen.getByText('0 messages')).toBeDefined();
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
    expect(within(detail).getByText('Code: timeout')).toBeDefined();
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

  it('shows one visible level-one heading in the narrow detail view', () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn()
    }));
    render(OperationsWorkspace, {
      controller: controller(snapshot({ detail: null, detailLoading: true })) as never,
      state: urlState({ operationRunID: RUN_ONE })
    });

    const headings = screen.getAllByRole('heading', { level: 1 });
    expect(headings.map((heading) => heading.textContent?.trim())).toEqual(['Operation detail']);
    expect(headings[0]!.closest('.kit-sr-only')).toBeNull();
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
