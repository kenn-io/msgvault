import type { ChipTone } from '@kenn-io/kit-ui';

import type { EmbeddingStatus, OperationLaneStatus, Status } from '../api/generated/models';
import type { CategoryID } from './layout';

/** What the one button on a row does. */
export type OverviewAction =
  | { kind: 'setup'; category: CategoryID; key?: string }
  | { kind: 'start'; id: 'messages' | 'visual_build' | 'visual_resume' | 'contacts'; label: string; consent?: string }
  | { kind: 'command'; command: string }
  | { kind: 'guide'; href: string }
  | { kind: 'sources' };

export interface OverviewRow {
  id: string;
  name: string;
  chip: { tone: ChipTone; text: string };
  action?: OverviewAction;
}

const BUILD_VISUAL_CONSENT = 'Sends attachments to your search provider.';

type Kind = OperationLaneStatus['kind'];

function lane(lanes: readonly OperationLaneStatus[], kind: Kind): OperationLaneStatus | undefined {
  return lanes.find((candidate) => candidate.kind === kind);
}

function failed(found: OperationLaneStatus | undefined): boolean {
  const state = found?.latest?.state;
  return !found?.active && (state === 'failed' || state === 'partial');
}

function count(value: unknown): number {
  return Number(value) || 0;
}

/**
 * One row per background feature, from the operation lanes plus the message and
 * visual index counts that Settings already loads.
 */
export function overviewRows(
  lanes: readonly OperationLaneStatus[],
  embedding: EmbeddingStatus | undefined,
  visual: Status | undefined,
): OverviewRow[] {
  // Contacts connections sync without the legacy single-account switch, so a
  // contacts lane that has run counts as on.
  const on = (kind: Kind): boolean => {
    const found = lane(lanes, kind);
    return Boolean(found?.configured || (kind === 'carddav_sync' && found?.latest));
  };
  const chip = (kind: Kind, building?: string): OverviewRow['chip'] => {
    const found = lane(lanes, kind);
    if (!found || found.unavailable_code) return { tone: 'muted', text: 'Unavailable' };
    if (found.active) return { tone: 'info', text: found.active.state === 'queued' ? 'Queued' : 'Running' };
    if (!on(kind)) return { tone: 'muted', text: 'Off' };
    if (failed(lane(lanes, kind))) return { tone: 'danger', text: 'Failed' };
    if (building) return { tone: 'info', text: building };
    return { tone: 'success', text: 'On' };
  };
  // Setting up a feature that needs semantic search starts at that switch.
  const search = (key: string): OverviewAction => ({
    kind: 'setup',
    category: 'search',
    key: on('message_embedding') ? key : 'vector.enabled',
  });

  const rows: OverviewRow[] = [];
  // A failed feature with no start action links to where it is set up.
  const setups: Record<string, OverviewAction | undefined> = {
    message_embedding: { kind: 'setup', category: 'search', key: 'vector.enabled' },
    person_embedding: search('vector.people.enabled'),
    visual_embedding: { kind: 'setup', category: 'search', key: 'vector.multimodal.enabled' },
    person_enrichment: { kind: 'setup', category: 'enrichment', key: 'people.enrichment.enabled' },
    person_sweep: { kind: 'setup', category: 'enrichment' },
    carddav_sync: { kind: 'setup', category: 'carddav' },
  };

  const messagesBuilt = Boolean(embedding?.active_generation);
  const messageProgress =
    embedding && messagesBuilt && count(embedding.pending) > 0
      ? `Building ${count(embedding.current).toLocaleString()} of ${count(embedding.eligible).toLocaleString()}`
      : undefined;
  const needsIndex = on('message_embedding') && embedding !== undefined && !messagesBuilt;
  rows.push({
    id: 'message_embedding',
    name: 'Message search',
    chip: needsIndex && !lane(lanes, 'message_embedding')?.active && !failed(lane(lanes, 'message_embedding')) ? { tone: 'muted', text: 'Unbuilt' } : chip('message_embedding', messageProgress),
    action: needsIndex && !lane(lanes, 'message_embedding')?.active
      ? { kind: 'start', id: 'messages', label: 'Build index' }
      : on('message_embedding')
        ? undefined
        : setups.message_embedding,
  });

  rows.push({
    id: 'person_embedding',
    name: 'People search',
    chip: chip('person_embedding'),
    action: on('person_embedding') ? undefined : setups.person_embedding,
  });

  const visualLane = lane(lanes, 'visual_embedding');
  const visualCurrent = count(visual?.current);
  const visualEligible = count(visual?.eligible);
  const visualProgress =
    visual && visualCurrent < visualEligible
      ? `Building ${visualCurrent.toLocaleString()} of ${visualEligible.toLocaleString()}`
      : undefined;
  const visualActions = visualLane?.supported_actions ?? [];
  rows.push({
    id: 'visual_embedding',
    name: 'Visual search',
    chip: chip('visual_embedding', visualProgress),
    action: !on('visual_embedding')
      ? setups.visual_embedding
      : visualActions.includes('visual_build')
        ? { kind: 'start', id: 'visual_build', label: 'Build visual index', consent: BUILD_VISUAL_CONSENT }
        : visualActions.includes('visual_resume')
          ? { kind: 'start', id: 'visual_resume', label: 'Resume visual index' }
          : undefined,
  });

  rows.push({
    id: 'person_enrichment',
    name: 'People lookups',
    chip: chip('person_enrichment'),
    action: on('person_enrichment') ? undefined : setups.person_enrichment,
  });

  rows.push({
    id: 'person_sweep',
    name: 'Facts and briefs',
    chip: chip('person_sweep'),
    action: on('person_sweep') ? undefined : setups.person_sweep,
  });

  rows.push({
    id: 'carddav_sync',
    name: 'Contacts',
    chip: chip('carddav_sync'),
    action: !on('carddav_sync')
      ? setups.carddav_sync
      : lane(lanes, 'carddav_sync')?.supported_actions.includes('carddav_sync')
        ? { kind: 'start', id: 'contacts', label: 'Sync now' }
        : undefined,
  });

  rows.push({
    id: 'document_extraction',
    name: 'Document extraction',
    chip: chip('document_extraction'),
    action: on('document_extraction')
      ? { kind: 'command', command: 'msgvault documents build' }
      : { kind: 'guide', href: 'https://msgvault.io/docs/usage/document-indexing/#configure-the-policy' },
  });
  rows.push({
    id: 'document_embedding',
    name: 'Document search',
    chip: chip('document_embedding'),
    action: on('document_embedding')
      ? { kind: 'command', command: 'msgvault documents vectors build' }
      : { kind: 'guide', href: 'https://msgvault.io/docs/usage/document-indexing/#semantic-and-hybrid-document-search' },
  });

  rows.unshift({ id: 'source_sync', name: 'Source sync', chip: chip('source_sync'), action: { kind: 'sources' } });
  return rows.map((row) =>
    !row.action && row.chip.text === 'Failed' && setups[row.id] ? { ...row, action: setups[row.id] } : row,
  );
}
