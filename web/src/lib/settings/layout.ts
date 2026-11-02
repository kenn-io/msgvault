import { groupSettings, type SettingGroupState, type SettingState } from './catalog';

/** Settings categories in navigation order. */
export const RAIL = [
  { id: 'overview', label: 'Overview' },
  { id: 'server', label: 'Server' },
  { id: 'search', label: 'Search & AI' },
  { id: 'enrichment', label: 'People lookups' },
  { id: 'tasks', label: 'Tasks' },
  { id: 'carddav', label: 'Contacts' },
  { id: 'sources', label: 'Sources' },
  { id: 'advanced', label: 'Advanced' },
] as const;

export type CategoryID = (typeof RAIL)[number]['id'];

/** A run of rows under an optional heading. An inner array puts related fields on one row. */
export interface Block {
  title?: string;
  description?: string;
  section?: string;
  rows: readonly (string | readonly string[])[];
}

/** The settings each category shows up front. Everything else it owns lands in Advanced. */
export const BLOCKS: Readonly<Partial<Record<CategoryID, readonly Block[]>>> = {
  server: [
    { rows: ['server.daemon_auto_start', 'server.daemon_idle_timeout', 'server.daemon_auto_restart'] },
    { title: 'Logs', rows: ['log.enabled', 'log.level'] },
  ],

  search: [
    { rows: ['vector.enabled'] },
    {
      title: 'Search provider',
      rows: [
        ['vector.embeddings.endpoint', 'vector.embeddings.model'],
        'vector.embeddings.api_key',
      ],
    },
    {
      section: 'people',
      rows: [
        'vector.people.enabled',
        ['vector.people.retention_posture', 'vector.people.training_posture'],
      ],
    },
    {
      section: 'visual',
      rows: [
        'vector.multimodal.enabled',
        'vector.multimodal.api_key',
      ],
    },
  ],
  enrichment: [{ rows: ['people.enrichment.enabled', 'people.enrichment.schedule'] }],
  tasks: [
    {
      title: 'Kata',
      rows: [
        'integrations.kata.enabled',
        ['integrations.kata.endpoint', 'integrations.kata.default_project'],
        'integrations.kata.api_key',
      ],
    },
    {
      title: 'Task service',
      rows: [
        'integrations.tasks.enabled',
        ['integrations.tasks.endpoint', 'integrations.tasks.default_project'],
        'integrations.tasks.api_key',
      ],
    },
  ],
  advanced: [{ title: 'Archive', rows: ['activity.timezone', 'activity.schedule'] }, { title: 'Appearance', rows: [['web.theme', 'web.density'], 'web.default_search_mode'] }],
};

/** Labels that say what a setting does in words a user knows. Other labels come from the daemon. */
export const LABELS: Readonly<Record<string, string>> = {
  'server.daemon_auto_start': 'Start automatically',
  'server.daemon_idle_timeout': 'Stop when idle after',
  'server.daemon_auto_restart': 'Restart after an update',
  'log.enabled': 'Keep logs',
  'log.sql_slow_ms': 'Slow SQL threshold (ms)',
  'inline.max_media_mb': 'Inline maximum size (MiB)',
  'beeper.max_media_mb': 'Beeper maximum size (MiB)',
  'slack.max_media_mb': 'Slack maximum size (MiB)',
  'discord.max_media_mb': 'Discord maximum size (MiB)',
  'teams.max_media_mb': 'Teams maximum size (MiB)',
  'vector.embeddings.endpoint': 'Endpoint',
  'vector.embeddings.model': 'Model',
  'vector.embeddings.api_key': 'API key',
  'vector.people.enabled': 'Search people',
  'vector.people.retention_posture': 'How long the provider keeps data',
  'vector.people.training_posture': 'Whether the provider trains on data',
  'vector.multimodal.enabled': 'Visual search',
  'vector.multimodal.api_key': 'Visual search API key',
  'people.enrichment.enabled': 'Look people up',
  'people.enrichment.schedule': 'Schedule',
  'integrations.kata.enabled': 'Kata tasks and agendas',
  'integrations.kata.endpoint': 'Address',
  'integrations.kata.default_project': 'Project',
  'integrations.kata.api_key': 'API key',
  'integrations.tasks.enabled': 'Link messages to tasks',
  'integrations.tasks.endpoint': 'Address',
  'integrations.tasks.default_project': 'Project',
  'integrations.tasks.api_key': 'API key',
  'web.default_search_mode': 'Search opens in',
};

/** Value names shown for options that would otherwise read as raw codes. */
export const OPTION_LABELS: Readonly<Record<string, Readonly<Record<string, string>>>> = {
  'server.daemon_auto_restart': { newer: 'On a new version', never: 'Never', always: 'Always' },
  'web.default_search_mode': { full_text: 'Exact words', hybrid: 'Best match' },
};

export const HIDDEN_OPTIONS: Readonly<Record<string, readonly string[]>> = {
  'web.default_search_mode': ['semantic'],
};

/** Every catalog field remains reachable in Settings. */
export function categoryOf(setting: SettingState): CategoryID {
  return setting.key.startsWith('carddav.') ? 'carddav' : normalizeCategory(setting.group, 'advanced');
}

export function normalizeCategory(id: string, fallback: CategoryID = 'overview'): CategoryID {
  const aliases: Record<string, CategoryID> = { browser: 'advanced', archive: 'advanced', integrations: 'tasks', attachments: 'sources', people: 'enrichment' };
  return RAIL.some(category => category.id === id) ? id as CategoryID : Object.hasOwn(aliases, id) ? aliases[id]! : fallback;
}

function flat(block: Block): string[] {
  return block.rows.flatMap((row) => (typeof row === 'string' ? [row] : [...row]));
}

export interface CategoryLayout {
  blocks: readonly Block[];
  /** Tuning that most people never touch, behind one disclosure. */
  advanced: SettingState[];
  /** Read-only values the host sets, behind one disclosure. */
  config: SettingState[];
}

/** Splits one category's settings into its up-front blocks, Advanced, and Set in config.toml. */
export function layoutCategory(id: CategoryID, settings: readonly SettingState[], groups: readonly SettingGroupState[] = []): CategoryLayout {
  const sourceGroups = id === 'sources' ? groupSettings(settings, groups).filter(group => group.id === 'sources' || group.id === 'attachments') : [];
  const blocks = sourceGroups.length > 0 ? sourceGroups.flatMap(group => {
    const rows = group.settings.filter(setting => !setting.section).map(setting => setting.key);
    // The Sources group's description only restates the category heading.
    return [
      group.id === 'sources' ? { rows } : { title: group.label, description: group.description, rows },
      ...group.sections.map(section => ({ title: section.label, rows: section.settings.map(setting => setting.key) })),
    ];
  }) : (BLOCKS[id] ?? []).map(block => ({
    ...block,
    description: block.description ?? groups.find(group => group.id === id)?.sections?.find(section => section.id === block.section)?.description,
  }));
  const shown = new Set(blocks.flatMap(flat));
  const own = settings.filter((setting) => categoryOf(setting) === id);
  return {
    blocks,
    advanced: own.filter((setting) => !setting.read_only && !shown.has(setting.key)),
    config: own.filter((setting) => setting.read_only),
  };
}
