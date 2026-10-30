import type { ChipTone } from '@kenn-io/kit-ui';
import type { OperationPublicCounter } from '../api/generated/models';
import type { SettingsNavigationAuthority } from '../carddav/navigation';
import { sentenceCase } from '../explore/labels';
import type { OperationAction, OperationKind, OperationLane, OperationLaneStatus, OperationRunSummary } from './models';

export type RelatedStatus = NonNullable<OperationLaneStatus['related_status']>;

export const OPERATION_LANE_LABELS: Readonly<Record<OperationLane, string>> = {
  messages: 'Messages', person_facts: 'Person facts', contacts: 'Contacts',
  visual_attachments: 'Attachments'
};
export const OPERATION_KIND_LABELS: Readonly<Record<OperationKind, string>> = {
  source_sync: 'Source sync', message_embedding: 'Message embedding', person_sweep: 'Person fact sweep',
  person_embedding: 'Person embedding', person_enrichment: 'Person enrichment', carddav_sync: 'CardDAV sync',
  visual_embedding: 'Visual embedding'
};
export const RELATED_STATUS_LABELS: Readonly<Record<RelatedStatus, string>> = {
  listSourceStatus: 'Sources status',
  getVisualAttachmentStatus: 'Visual attachment status',
  getCardDAVStatus: 'CardDAV settings'
};
export const OPERATION_ACTION_LABELS: Readonly<Record<OperationAction, string>> = {
  carddav_sync: 'Start CardDAV sync', visual_build: 'Build visual index', visual_resume: 'Resume visual index'
};

export interface OperationSettingsTarget {
  settingsCategory: string;
  settingsAuthority: SettingsNavigationAuthority | '';
}
export interface OperationHostSetup {
  kind: 'host';
  text: string;
  guideLabel: string;
  guideHref: string;
}
export type OperationSetup = { kind: 'settings'; target: OperationSettingsTarget } | OperationHostSetup;

// Where an Off kind is turned on (spec "Set up targets"). source_sync and
// carddav_sync have no entry: their related-status buttons already lead there.
export const OPERATION_SETUP: Readonly<Partial<Record<OperationKind, OperationSetup>>> = {
  message_embedding: {
    kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' }
  },
  person_embedding: {
    kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'person_embeddings' }
  },
  visual_embedding: {
    kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'visual_attachments' }
  },
  person_enrichment: { kind: 'settings', target: { settingsCategory: 'enrichment', settingsAuthority: '' } },
  person_sweep: { kind: 'settings', target: { settingsCategory: 'people', settingsAuthority: '' } },
};

const STATE_CHIPS: Readonly<Record<string, { label: string; tone: ChipTone }>> = {
  queued: { label: 'Queued', tone: 'info' },
  running: { label: 'Running', tone: 'info' },
  succeeded: { label: 'Succeeded', tone: 'success' },
  partial: { label: 'Partial', tone: 'warning' },
  failed: { label: 'Failed', tone: 'danger' },
  cancelled: { label: 'Cancelled', tone: 'muted' }
};

export function operationStateChip(state: string): { label: string; tone: ChipTone } {
  return Object.hasOwn(STATE_CHIPS, state) ? STATE_CHIPS[state]! : { label: sentenceCase(state), tone: 'neutral' };
}

export function triggerLabel(trigger: string | undefined): string {
  if (!trigger) return '—';
  if (trigger === 'manual') return 'Manual';
  if (trigger === 'scheduled') return 'Scheduled';
  return sentenceCase(trigger);
}

// Units come from the closed counter registry in internal/operations/types.go.
const UNIT_SINGULAR: Readonly<Record<string, string>> = {
  messages: 'message', people: 'person', writes: 'write', books: 'book', contacts: 'contact',
  attachments: 'attachment'
};

function unitWord(unit: string, value: number): string {
  return value === 1 && Object.hasOwn(UNIT_SINGULAR, unit) ? UNIT_SINGULAR[unit]! : unit;
}

function counterPhrase(counter: OperationPublicCounter, nameUnit: boolean): string {
  const value = counter.value.toLocaleString();
  const words = counter.name.replaceAll('_', ' ');
  // A counter named for its unit ("books", "projected writes") already says what it counts.
  if (words.split(' ').at(-1) === counter.unit) {
    return `${value} ${words.replace(/\S+$/, unitWord(counter.unit, counter.value))}`;
  }
  return nameUnit ? `${value} ${unitWord(counter.unit, counter.value)} ${words}` : `${value} ${words}`;
}

export function counterSummary(counters: readonly OperationPublicCounter[]): string {
  const namedUnits = new Set<string>();
  const parts: string[] = [];
  for (const counter of counters) {
    if (counter.value === 0) continue;
    parts.push(counterPhrase(counter, !namedUnits.has(counter.unit)));
    namedUnits.add(counter.unit);
  }
  return parts.length > 0 ? parts.join(' · ') : 'No counters';
}

export const counterLabel = (name: string): string => sentenceCase(name);

export function counterValue(counter: OperationPublicCounter): string {
  return `${counter.value.toLocaleString()} ${unitWord(counter.unit, counter.value)}`;
}

function plural(value: number, unit: string): string {
  return `${value} ${value === 1 ? unit : `${unit}s`}`;
}

export function operationDuration(run: Pick<OperationRunSummary, 'state' | 'started_at' | 'finished_at'>): string {
  if (!run.finished_at) return run.state === 'running' ? 'In progress' : 'Not available';
  const milliseconds = Date.parse(run.finished_at) - Date.parse(run.started_at);
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return 'Not available';
  const totalSeconds = Math.floor(milliseconds / 1_000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return plural(seconds, 'second');
  return seconds ? `${plural(minutes, 'minute')} ${plural(seconds, 'second')}` : plural(minutes, 'minute');
}
