import { describe, expect, it } from 'vitest';

import type { EmbeddingStatus, OperationLaneStatus } from '../api/generated/models';
import { overviewRows } from './overview';

function lane(kind: OperationLaneStatus['kind'], extra: Partial<OperationLaneStatus> = {}): OperationLaneStatus {
  return { kind, configured: true, lane: 'messages', history_availability: 'available', supported_actions: [], ...extra } as OperationLaneStatus;
}

function row(rows: ReturnType<typeof overviewRows>, id: string) {
  const found = rows.find((candidate) => candidate.id === id);
  if (!found) throw new Error(`no row ${id}`);
  return found;
}

describe('overviewRows', () => {
  it('lists every background feature, off ones with a way to turn them on', () => {
    const rows = overviewRows([lane('message_embedding', { configured: false })], undefined, undefined);

    expect(rows.map((candidate) => candidate.name)).toEqual([
      'Source sync',
      'Message search',
      'People search',
      'Visual search',
      'People lookups',
      'Facts and briefs',
      'Contacts',
      'Document extraction',
      'Document search',
    ]);
    expect(row(rows, 'message_embedding')).toMatchObject({ chip: { text: 'Off' }, action: { kind: 'settings', target: { settingsCategory: 'search' } } });
    expect(row(rows, 'document_extraction').action).toMatchObject({ kind: 'host' });
  });

  it('uses server coverage actions even for an already built index', () => {
    const rows = overviewRows([lane('message_embedding')], { active_generation: 1 } as unknown as EmbeddingStatus, undefined, ['build_index']);
    expect(row(rows, 'message_embedding').action).toMatchObject({ kind: 'start', id: 'messages', label: 'Build index' });
  });

  it('withholds a duplicate first build before its lane run appears', () => {
    const rows = overviewRows([lane('message_embedding')], { active_generation: null } as unknown as EmbeddingStatus, undefined, []);
    expect(row(rows, 'message_embedding').action).toBeUndefined();
  });

  it('offers the visual build the server advertises, with its consent line', () => {
    const rows = overviewRows([lane('visual_embedding', { supported_actions: ['visual_build'] })], undefined, undefined);

    expect(row(rows, 'visual_embedding').action).toMatchObject({ kind: 'start', id: 'visual_build', consent: expect.any(String) });
  });

  it('keeps disabled contacts Off after an earlier successful sync', () => {
    const rows = overviewRows([lane('carddav_sync', { configured: false, latest: { state: 'succeeded' } } as Partial<OperationLaneStatus>)], undefined, undefined);

    expect(row(rows, 'carddav_sync').chip.text).toBe('Off');
    expect(row(rows, 'carddav_sync').action).toMatchObject({ kind: 'settings', target: { settingsCategory: 'carddav' } });
  });

  it('marks a configured failed feature without offering Set up', () => {
    const rows = overviewRows([lane('person_enrichment', { latest: { state: 'failed' } } as Partial<OperationLaneStatus>)], undefined, undefined);

    expect(row(rows, 'person_enrichment').chip.text).toBe('Failed');
    expect(row(rows, 'person_enrichment').action).toBeUndefined();
  });
});

it('distinguishes active work from an unbuilt index', () => {
  const active = { id: 'run-1', kind: 'message_embedding', lane: 'messages', state: 'running', started_at: '2026-01-01T00:00:00Z', counters: [] } as const;
  const rows = overviewRows([lane('message_embedding', { active: { ...active, counters: [] } })], { active_generation: null } as unknown as EmbeddingStatus, undefined);
  expect(row(rows, 'message_embedding').chip.text).toBe('Running');
  expect(row(rows, 'message_embedding').action).toBeUndefined();
});

it('offers only the advertised contacts sync action', () => {
  expect(row(overviewRows([lane('carddav_sync')], undefined, undefined), 'carddav_sync').action).toBeUndefined();
  expect(row(overviewRows([lane('carddav_sync', { supported_actions: ['carddav_sync'] })], undefined, undefined), 'carddav_sync').action).toEqual({ kind: 'start', id: 'contacts', label: 'Sync now' });
});

it('shows index progress only while a run is active', () => {
  const embedding = { active_generation: 1, current: 2, eligible: 10, pending: 8 } as unknown as EmbeddingStatus;
  const visual = { current: 3, eligible: 10 } as import('../api/generated/models').Status;
  const active = { id: 'run-1', state: 'running', counters: [] } as unknown as OperationLaneStatus['active'];
  for (const kind of ['message_embedding', 'visual_embedding'] as const) {
    expect(row(overviewRows([lane(kind)], embedding, visual), kind).chip.text).toBe('On');
    expect(row(overviewRows([lane(kind, { active })], embedding, visual), kind).chip.text).toBe(`Building ${kind === 'message_embedding' ? 2 : 3} of 10`);
  }
});

it.each(['message_embedding', 'person_embedding', 'document_extraction', 'document_embedding', 'visual_embedding', 'person_enrichment'] as const)('uses current configuration when %s history is unavailable', kind => {
  for (const configured of [true, false]) {
    const rows = overviewRows([lane(kind, { configured, history_availability: 'unavailable', unavailable_code: `${kind}_history_unavailable` })], undefined, undefined);
    expect(row(rows, kind).chip.text).toBe(configured ? 'On' : 'Off');
  }
});

it('shows a partial latest run with the shared warning label and tone', () => {
  const rows = overviewRows([lane('person_enrichment', { latest: { state: 'partial' } } as Partial<OperationLaneStatus>)], undefined, undefined);
  expect(row(rows, 'person_enrichment').chip).toEqual({ text: 'Partial', tone: 'warning' });
  expect(row(rows, 'person_enrichment').action).toBeUndefined();
});
