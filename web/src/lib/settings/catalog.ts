import type { components } from '../api/generated/schema';

export type SettingState = components['schemas']['Setting'];
export type SettingKind = SettingState['kind'];

export type SecretSettingState = components['schemas']['SecretSettingState'];

export type SettingValue = components['schemas']['SettingValue'];
export type SettingsDocument = components['schemas']['SettingsResponse'];
export type SettingGroupState = components['schemas']['SettingGroup'];

export interface CatalogEntry {
  group: SettingGroupID;
  label: string;
  description: string;
  secret?: boolean;
  testable?: boolean;
  options?: string[];
}

export type SettingGroupID =
  | 'browser'
  | 'server'
  | 'archive'
  | 'search'
  | 'sources'
  | 'integrations';

export const settingGroups: ReadonlyArray<{ id: SettingGroupID; label: string }> = [
  { id: 'browser', label: 'Browser experience' },
  { id: 'server', label: 'Server access' },
  { id: 'archive', label: 'Archive and cache' },
  { id: 'search', label: 'Search and vectors' },
  { id: 'sources', label: 'Source schedules' },
  { id: 'integrations', label: 'Optional integrations' }
];

export const settingsCatalog: Record<string, CatalogEntry> = {
  'web.default_search_mode': entry('browser', 'Default search mode', 'Mode used when a URL does not choose one.', {
    options: ['full_text', 'semantic', 'hybrid']
  }),
  'web.theme': entry('browser', 'Theme', 'Browser color theme.', {
    options: ['system', 'light', 'dark']
  }),
  'web.density': entry('browser', 'Density', 'Table and toolbar spacing.', {
    options: ['compact', 'comfortable']
  }),
  'server.bind_addr': entry('server', 'Bind address', 'Address the daemon listens on.'),
  'server.api_port': entry('server', 'API port', 'Zero lets the daemon select an available port.'),
  'server.api_key': entry('server', 'Daemon API key', 'Key used by remote clients and browser login.', {
    secret: true
  }),
  'server.allow_insecure': entry('server', 'Allow insecure access', 'Permit unauthenticated non-loopback access.'),
  'server.trusted_proxies': entry('server', 'Trusted proxies', 'IP addresses or CIDR ranges allowed to supply forwarded HTTPS metadata.'),
  'analytics.engine': entry('archive', 'Analytics engine', 'Engine used for aggregate queries.', {
    options: ['auto', 'sql', 'duckdb']
  }),
  'analytics.auto_build_cache': entry('archive', 'Build stale cache automatically', 'Build Parquet analytics before cached queries.'),
  'vector.enabled': entry('search', 'Semantic search', 'Enable the vector subsystem.'),
  'vector.backend': entry('search', 'Vector backend', 'Storage backend for embeddings.', {
    options: ['sqlite-vec', 'pgvector']
  }),
  'vector.db_path': entry('search', 'Vector database path', 'Optional vector database path override.'),
  'vector.skip_extension_create': entry('search', 'Skip extension creation', 'Use a vector extension installed by an administrator.'),
  'vector.embeddings.api_format': entry(
    'search',
    'Embedding API format',
    'Select the OpenAI-compatible or Voyage contextual API.',
    { options: ['openai', 'voyage-contextual'] }
  ),
  'vector.embeddings.endpoint': entry('search', 'Embedding endpoint', 'OpenAI-compatible or Voyage API base URL.'),
  'vector.embeddings.api_key_env': entry('search', 'Embedding key environment variable', 'Environment variable containing the endpoint key.'),
  'vector.embeddings.model': entry('search', 'Embedding model', 'Model identifier used to build an index generation.'),
  'vector.embeddings.document_prefix': entry('search', 'Document task prefix', 'Text prepended to every document chunk.'),
  'vector.embeddings.query_prefix': entry('search', 'Query task prefix', 'Text prepended to every vector search query.'),
  'vector.embeddings.dimension': entry('search', 'Embedding dimension', 'Vector dimension returned by the model.'),
  'vector.embeddings.batch_size': entry('search', 'Embedding batch size', 'Items sent per embedding request.'),
  'vector.embeddings.max_retries': entry('search', 'Embedding retries', 'Maximum transient request retries.'),
  'vector.embeddings.max_input_chars': entry('search', 'Maximum input characters', 'Per-chunk text limit.'),
  'vector.embeddings.eta_window': entry('search', 'ETA window', 'Recent samples used for ETA smoothing.'),
  'vector.people.enabled': entry(
    'search',
    'Semantic person embeddings',
    'Allow exact-consented curated fields for every durable person to use the embedding provider.'
  ),
  'vector.people.retention_posture': entry(
    'search',
    'Person embedding retention assertion',
    'Operator assertion describing provider retention for curated person embeddings.'
  ),
  'vector.people.training_posture': entry(
    'search',
    'Person embedding training assertion',
    'Operator assertion describing provider training use for curated person embeddings.'
  ),
  'vector.embed.schedule.cron': entry('search', 'Embedding schedule', 'Cron schedule for background embedding.'),
  'vector.embed.schedule.run_after_sync': entry('search', 'Embed after sync', 'Run embedding after a successful source sync.'),
  'vector.embed.scope.message_types': entry('search', 'Embedded message types', 'Optional message-type scope.'),
  'vector.embed.scope.accounts': entry(
    'search',
    'Embedded accounts',
    'Optional account scope; only listed accounts are embedded.'
  ),
  'vector.multimodal.enabled': entry('search', 'Visual attachment search', 'Independently enable hosted visual attachment indexing.'),
  'vector.multimodal.provider': entry('search', 'Visual embedding provider', 'Provider used for visual attachment embeddings.', {
    options: ['voyage']
  }),
  'vector.multimodal.endpoint': entry('search', 'Visual embedding endpoint', 'HTTPS API root used for multimodal embedding requests.'),
  'vector.multimodal.api_key_env': entry('search', 'Visual embedding key environment variable', 'Local environment variable containing the provider key.'),
  'vector.multimodal.capabilities_file': entry('search', 'Visual capability manifest', 'Path to the operator-probed Voyage capability manifest.'),
  'vector.multimodal.model': entry('search', 'Visual embedding model', 'Model identifier used for visual generations.', {
    options: ['voyage-multimodal-3.5']
  }),
  'vector.multimodal.dimension': entry('search', 'Visual embedding dimension', 'Vector dimension required by the multimodal model.'),
  'vector.multimodal.max_context_chars': entry('search', 'Maximum visual context characters', 'Hard limit for normalized owning-message context.'),
  'vector.multimodal.include_images': entry('search', 'Index still images', 'Allow authoritative standalone still images to leave the machine for indexing.'),
  'vector.multimodal.include_animated_gifs': entry('search', 'Index animated GIFs', 'Allow animated GIFs only after provider capability is confirmed.'),
  'vector.multimodal.include_video': entry('search', 'Index videos', 'Allow bounded authoritative standalone videos to leave the machine for indexing.'),
  'vector.multimodal.allow_image_queries': entry('search', 'Allow image queries', 'Permit bounded query images to be sent to the visual embedding provider.'),
  'vector.multimodal.scope.message_types': entry('search', 'Visual message types', 'Optional message-type scope for visual indexing.'),
  'vector.multimodal.schedule.cron': entry('search', 'Visual indexing schedule', 'Cron schedule for background visual indexing.'),
  'vector.multimodal.schedule.run_after_sync': entry('search', 'Index visuals after sync', 'Run visual indexing after a successful source sync.'),
  'vector.search.rrf_k': entry('search', 'RRF constant', 'Reciprocal rank fusion constant.'),
  'vector.search.k_per_signal': entry('search', 'Candidates per signal', 'Candidate count contributed by each hybrid signal.'),
  'vector.search.subject_boost': entry('search', 'Subject boost', 'Hybrid ranking weight for subject matches.'),
  'beeper.enabled': entry('sources', 'Desktop chat schedule enabled', 'Enable scheduled imports from the supported desktop chat source.'),
  'beeper.schedule': entry('sources', 'Desktop chat schedule', 'Cron schedule for the supported desktop chat source.'),
  'integrations.tasks.enabled': entry('integrations', 'Task integration', 'Enable the provider-neutral task integration.'),
  'integrations.tasks.endpoint': entry('integrations', 'Task endpoint', 'HTTPS, loopback, Unix socket, or local discovery endpoint.'),
  'integrations.tasks.api_key': entry('integrations', 'Task integration API key', 'Bearer key used only by the daemon.', {
    secret: true
  }),
  'integrations.tasks.default_project': entry('integrations', 'Default task project', 'Project used for task creation and lookup.')
};

export interface SettingsGroup {
  id: string;
  label: string;
  description: string;
  settings: SettingState[];
}

export function isManagedSetting(key: string): boolean {
  return Object.hasOwn(settingsCatalog, key);
}

export function groupSettings(
  settings: SettingState[],
  daemonGroups: ReadonlyArray<SettingGroupState> = []
): SettingsGroup[] {
  if (daemonGroups.length > 0) {
    return daemonGroups
      .map((group) => ({
        id: group.id,
        label: group.label,
        description: group.description,
        settings: settings.filter((setting) => setting.group === group.id)
      }))
      .filter((group) => group.settings.length > 0);
  }
  return settingGroups
    .map((group) => ({
      ...group,
      description: 'Changes are written to config.toml and use optimistic concurrency.',
      settings: settings.filter(
        (setting) => isManagedSetting(setting.key) && settingsCatalog[setting.key]?.group === group.id
      )
    }))
    .filter((group) => group.settings.length > 0);
}

function entry(
  group: SettingGroupID,
  label: string,
  description: string,
  options: Partial<CatalogEntry> = {}
): CatalogEntry {
  return { group, label, description, ...options };
}
