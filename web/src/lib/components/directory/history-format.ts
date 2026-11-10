import { sentenceCase } from '../../explore/labels';
import { formatDateTime, formatShortDate } from '../../util/format';

type HistoryValue = { source: string; source_ref?: string; active_from?: string; active_until?: string; superseded_at?: string; created_at: string; updated_at?: string; actor?: string; confidence?: number };

/** A provenance line under a value, and its tooltip with every timestamp the record has. */
export interface ProvenanceLine { text: string; title: string }

export interface ProvenanceOptions {
  /** What was done, for a line about an act: "Merged" reads "Merged by you". */
  action?: string;
  /** The label for `at` in the tooltip; defaults to the action, then "Recorded". */
  atLabel?: string;
  /** The record's other timestamps, shown only in the tooltip. */
  times?: [label: string, at: string | undefined][];
}

// Who acted, for an actor msgvault records rather than a source: the owner
// through the app, the CLI, or the API reads "you"; msgvault's own work reads "msgvault".
const ACTOR: Record<string, string> = {
  user: 'you', api: 'you', cli: 'you', local_operator: 'you', operator: 'you', reviewer: 'you',
  system: 'msgvault', msgvault: 'msgvault', importer: 'msgvault', automatic: 'msgvault',
  mcp: 'an assistant',
};

/** Who acted, as a line names them: "you", "msgvault", or the actor as recorded. */
export function actorName(actor: string): string {
  return ACTOR[actor] ?? actor;
}

const WHO: Record<string, string> = {
  carddav_import: 'From CardDAV contacts',
  vcard_import: 'From a contact file',
  archive_observation: 'From your messages',
  extraction: 'From a message',
  enrichment: 'From a lookup',
  brief: 'From a brief',
};

function sourceWho(source: string): string {
  const actor = ACTOR[source];
  return WHO[source] ?? (actor ? (actor === 'msgvault' ? actor : actor[0].toUpperCase() + actor.slice(1)) : sentenceCase(source));
}

function dayOf(at: string): string {
  const date = new Date(at);
  return Number.isNaN(date.valueOf()) ? at : date.toDateString();
}

/**
 * "{who} · {when}": who wrote a value and when, under the value it qualifies.
 * A value that became true on an earlier day than it was recorded reads
 * "since {validFrom}", unless the line names an act. The full timestamps go in
 * the tooltip, never on the line.
 */
export function provenance(source: string, at: string, validFrom?: string, options: ProvenanceOptions = {}): ProvenanceLine {
  const who = options.action && ACTOR[source] ? `${options.action} by ${ACTOR[source]}` : sourceWho(source);
  // A line about an act dates the act; validity then lives only in the tooltip.
  const earlier = !options.action && validFrom && new Date(validFrom) < new Date(at) && dayOf(validFrom) !== dayOf(at);
  const title = [
    `${options.atLabel ?? options.action ?? 'Recorded'} ${formatDateTime(at)}`,
    validFrom && validFrom !== at ? `Valid from ${formatDateTime(validFrom)}` : undefined,
    ...(options.times ?? []).map(([label, value]) => value ? `${label} ${formatDateTime(value)}` : undefined),
  ].filter(Boolean).join('\n');
  return { text: `${who} · ${earlier ? `since ${formatShortDate(validFrom)}` : formatShortDate(at)}`, title };
}

/** The provenance line for a stored value, from its envelope. */
export function valueProvenance(value: HistoryValue, times: [label: string, at: string | undefined][] = []): ProvenanceLine {
  const line = provenance(value.source, value.updated_at ?? value.created_at, value.active_from, {
    atLabel: value.updated_at ? 'Updated' : 'Recorded',
    times: [
      ['Recorded', value.updated_at ? value.created_at : undefined],
      ['Valid until', value.active_until],
      ['Replaced', value.superseded_at],
      ...times,
    ],
  });
  line.title = [line.title, value.actor ? `Actor: ${actorName(value.actor)}` : '', value.source_ref ? `Reference: ${value.source_ref}` : '', value.confidence !== undefined ? `Confidence: ${value.confidence}` : ''].filter(Boolean).join('\n');
  return line;
}

/** Who wrote a value: "You" for edits made here, otherwise the importer. */
export function historySource(source: string): string {
  return source === 'user' ? 'You' : source === 'carddav_import' ? 'Address book' : sentenceCase(source);
}
