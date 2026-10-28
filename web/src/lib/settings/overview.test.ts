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

  it('offers Build index when message search is on but has no index yet', () => {
    const rows = overviewRows([lane('message_embedding')], { active_generation: null } as unknown as EmbeddingStatus, undefined);

    expect(row(rows, 'message_embedding').action).toMatchObject({ kind: 'start', id: 'messages', label: 'Build index' });
  });

  it('offers the visual build the server advertises, with its consent line', () => {
    const rows = overviewRows([lane('visual_embedding', { supported_actions: ['visual_build'] })], undefined, undefined);

    expect(row(rows, 'visual_embedding').action).toMatchObject({ kind: 'start', id: 'visual_build', consent: expect.any(String) });
  });

  it('counts contacts as on once a connection has synced', () => {
    const rows = overviewRows([lane('carddav_sync', { configured: false, latest: { state: 'succeeded' } } as Partial<OperationLaneStatus>)], undefined, undefined);

    expect(row(rows, 'carddav_sync').chip.text).toBe('On');
  });

  it('marks a failed feature and links to where it is set up', () => {
    const rows = overviewRows([lane('person_enrichment', { latest: { state: 'failed' } } as Partial<OperationLaneStatus>)], undefined, undefined);

    expect(row(rows, 'person_enrichment')).toMatchObject({ chip: { text: 'Failed' }, action: { kind: 'settings', target: { settingsCategory: 'enrichment' } } });
  });
});

it('distinguishes unavailable status and active work from an unbuilt index', () => {
  expect(row(overviewRows([], undefined, undefined), 'person_sweep').chip.text).toBe('Unavailable');
  const active = { id: 'run-1', kind: 'message_embedding', lane: 'messages', state: 'running', started_at: '2026-01-01T00:00:00Z', counters: [] } as const;
  const rows = overviewRows([lane('message_embedding', { active: { ...active, counters: [] } })], { active_generation: null } as unknown as EmbeddingStatus, undefined);
  expect(row(rows, 'message_embedding').chip.text).toBe('Running');
  expect(row(rows, 'message_embedding').action).toBeUndefined();
});

it('offers only the advertised contacts sync action', () => {
  expect(row(overviewRows([lane('carddav_sync')], undefined, undefined), 'carddav_sync').action).toBeUndefined();
  expect(row(overviewRows([lane('carddav_sync', { supported_actions: ['carddav_sync'] })], undefined, undefined), 'carddav_sync').action).toEqual({ kind: 'start', id: 'contacts', label: 'Sync now' });
});
