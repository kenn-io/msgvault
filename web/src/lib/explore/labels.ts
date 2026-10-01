import { groupingDimensionLabel, isGroupingDimension } from '../grouping/catalog';
import type {
  ExploreColumn,
  ExploreFilterDimension,
  ExploreGroupDimension,
  ExploreSearchMode,
  ExploreURLState,
  FileMIMEFamily
} from './models';

const SEARCH_MODES: Record<ExploreSearchMode, string> = {
  full_text: 'Full text',
  semantic: 'Semantic',
  hybrid: 'Hybrid'
};

const PRESENTATIONS: Record<ExploreURLState['presentation'], string> = {
  table: 'Table',
  timeline: 'Timeline',
  files: 'Files'
};

// Canonical column order: the table renders visible columns in this order.
export const EXPLORE_COLUMNS: ReadonlyArray<{ id: ExploreColumn; label: string }> = [
  { id: 'kind', label: 'Kind' },
  { id: 'people', label: 'People / source' },
  { id: 'title', label: 'Subject / title' },
  { id: 'excerpt', label: 'Excerpt' },
  { id: 'time', label: 'Time' },
  { id: 'attachments', label: 'Attachments' },
  { id: 'size', label: 'Size' }
];

// Where the grouping label reads wrongly as a filter chip (plural, or a different word),
// name it here.
const FILTER_DIMENSIONS: Partial<Record<string, string>> = {
  participant: 'Person',
  identity: 'Identity',
  domain: 'Domain',
  message_type: 'Message type',
  mailing_list: 'Mailing list',
  after: 'After',
  before: 'Before',
  deletion: 'Deletion'
};

export const FILE_FAMILY_LABELS: Record<FileMIMEFamily, string> = {
  image: 'Images',
  pdf: 'PDFs',
  audio: 'Audio',
  video: 'Video',
  text: 'Text',
  document: 'Documents',
  archive: 'Archives',
  other: 'Other'
};

const FAMILY_SINGULAR: Record<FileMIMEFamily, string> = {
  image: 'Image',
  pdf: 'PDF',
  audio: 'Audio',
  video: 'Video',
  text: 'Text',
  document: 'Document',
  archive: 'Archive',
  other: 'File'
};

const KNOWN_MIME: Record<string, string> = {
  'application/pdf': 'PDF',
  'application/zip': 'ZIP archive',
  'application/msword': 'Word document',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document': 'Word document',
  'application/vnd.ms-excel': 'Excel spreadsheet',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet': 'Excel spreadsheet',
  'application/vnd.ms-powerpoint': 'PowerPoint presentation',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation':
    'PowerPoint presentation',
  'text/plain': 'Text',
  'text/html': 'HTML',
  'text/csv': 'CSV',
  'text/calendar': 'Calendar invite',
  'message/rfc822': 'Email message'
};

// Keys are `${action}:${reason}`, matching the unavailable_actions the preflight endpoint returns.
const REASONS: Record<string, string> = {
  'open_in_source:trusted_source_link_unavailable':
    'Your sources don’t provide links to open these items.',
  'export:browser_export_requires_single_message': 'Export works for one message at a time.',
  'export:selection_has_no_exportable_raw_message':
    'The selection has no original message to export.',
  'export:raw_message_unavailable': 'The original message isn’t available.',
  'export_files:selection_contains_no_files': 'The selection has no files.',
  'stage_deletion:selection_contains_items_that_cannot_be_deleted_from_source':
    'None of the selected items can be deleted from their source.'
};

export function sentenceCase(code: string): string {
  const words = code.replace(/[_-]+/g, ' ').trim();
  return words ? words[0]!.toUpperCase() + words.slice(1) : '';
}

export function searchModeLabel(mode: ExploreSearchMode): string {
  return SEARCH_MODES[mode];
}

export function presentationLabel(presentation: ExploreURLState['presentation']): string {
  return PRESENTATIONS[presentation];
}

export function filterDimensionLabel(
  dimension: ExploreFilterDimension | ExploreGroupDimension
): string {
  const explicit = FILTER_DIMENSIONS[dimension];
  if (explicit) return explicit;
  if (isGroupingDimension(dimension)) return groupingDimensionLabel(dimension);
  return sentenceCase(dimension);
}

// One wording for grouping in chips and saved-view summaries ("Grouped by Person, then Year").
export function groupedByLabel(dimensions: readonly string[]): string {
  const names = dimensions.map((dimension) =>
    filterDimensionLabel(dimension as ExploreFilterDimension | ExploreGroupDimension)
  );
  return `Grouped by ${names.join(', then ')}`;
}

export function fileTypeLabel(
  mimeType: string | undefined,
  family: string | undefined
): string {
  const mime = mimeType?.toLowerCase().split(';')[0]?.trim() ?? '';
  const known = KNOWN_MIME[mime];
  if (known) return known;
  const [kind, subtype] = mime.split('/');
  if ((kind === 'image' || kind === 'audio' || kind === 'video') && subtype) {
    return `${subtype.replace(/^x-/, '').toUpperCase()} ${kind}`;
  }
  return family && Object.hasOwn(FAMILY_SINGULAR, family)
    ? FAMILY_SINGULAR[family as FileMIMEFamily]
    : 'Unknown type';
}

export function preflightReasonLabel(action: string, reason: string): string {
  return REASONS[`${action}:${reason}`] ?? `${sentenceCase(reason)}.`;
}

interface EntryKindPresentation {
  icon: string;
  /** Full name, used as the accessible label ("Email item"). */
  label: string;
  /** Short visible name ("Email"). */
  name: string;
}

function kindPresentation(icon: string, label: string): EntryKindPresentation {
  return { icon, label, name: label.replace(' item', '').replace('Archive', 'Item') };
}

// Mirrors identityindex.TextMessageTypes plus its chat/text fallbacks
// (internal/identityindex/schema.go).
const CHAT_MESSAGE_TYPES = new Set([
  'chat', 'text', 'google_chat', 'whatsapp', 'imessage', 'sms', 'mms', 'rcs',
  'google_voice_text', 'teams', 'discord', 'beeper', 'slack', 'fbmessenger'
]);

// The server-assigned kind wins; the message type covers rows and archive messages without one.
export function entryKindPresentation(kind: string, messageType = ''): EntryKindPresentation {
  const normalizedKind = kind.toLowerCase();
  if (normalizedKind === 'email') return kindPresentation('✉', 'Email item');
  if (normalizedKind === 'conversation') return kindPresentation('◌', 'Conversation item');
  if (normalizedKind === 'event') return kindPresentation('□', 'Calendar event');
  if (normalizedKind === 'meeting') return kindPresentation('◫', 'Meeting item');
  if (normalizedKind === 'file') return kindPresentation('▱', 'File item');
  const normalized = messageType.toLowerCase();
  if (normalized === 'email') return kindPresentation('✉', 'Email item');
  if (CHAT_MESSAGE_TYPES.has(normalized)) return kindPresentation('◌', 'Conversation item');
  if (normalized === 'calendar' || normalized === 'calendar_event') {
    return kindPresentation('□', 'Calendar event');
  }
  if (normalized === 'meeting' || normalized === 'meeting_transcript') {
    return kindPresentation('◫', 'Meeting item');
  }
  return kindPresentation('◇', 'Archive item');
}
