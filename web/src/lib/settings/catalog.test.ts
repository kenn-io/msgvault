import { describe, expect, it } from 'vitest';

import { groupSettings, isManagedSetting, settingsCatalog, type SettingState } from './catalog';

describe('settings catalog', () => {
  it('contains the supported browser, server, archive, search, source and integration groups', () => {
    expect(settingsCatalog['web.default_search_mode'].options).toEqual([
      'full_text',
      'semantic',
      'hybrid'
    ]);
    expect(settingsCatalog['server.trusted_proxies'].group).toBe('server');
    expect(settingsCatalog['analytics.auto_build_cache'].group).toBe('archive');
    expect(settingsCatalog['vector.embeddings.endpoint'].testable).toBeUndefined();
    expect(settingsCatalog['vector.embeddings.api_format'].options).toEqual([
      'openai',
      'voyage-contextual'
    ]);
    expect(settingsCatalog['vector.embeddings.document_prefix'].description).toContain('document chunk');
    expect(settingsCatalog['vector.embeddings.query_prefix'].description).toContain('search query');
    expect(settingsCatalog['vector.embed.scope.accounts'].group).toBe('search');
    expect(settingsCatalog['vector.multimodal.enabled'].group).toBe('search');
    expect(settingsCatalog['vector.multimodal.provider'].options).toEqual(['voyage']);
    expect(settingsCatalog['vector.people.enabled'].description).toContain('every durable person');
    expect(settingsCatalog['vector.people.retention_posture'].group).toBe('search');
    expect(settingsCatalog['vector.people.training_posture'].group).toBe('search');
    expect(settingsCatalog['beeper.schedule'].group).toBe('sources');
    expect(settingsCatalog['integrations.tasks.api_key'].secret).toBe(true);
  });

  it('filters unknown keys and groups known settings in task order', () => {
    const settings: SettingState[] = [
      setting('integrations.tasks.enabled', false),
      setting('unsupported.private_value', 'hidden'),
      setting('web.theme', 'dark'),
      setting('server.bind_addr', '127.0.0.1')
    ];

    expect(isManagedSetting('web.theme')).toBe(true);
    expect(isManagedSetting('unsupported.private_value')).toBe(false);
    expect(groupSettings(settings).map((group) => group.id)).toEqual([
      'browser',
      'server',
      'integrations'
    ]);
    expect(groupSettings(settings).flatMap((group) => group.settings.map((item) => item.key))).not.toContain(
      'unsupported.private_value'
    );
  });


  it('uses daemon groups, ordering and metadata as the authoritative catalog', () => {
    const settings = [
      {
        ...setting('beeper.max_media_mb', 0),
        group: 'attachments',
        label: 'Daemon attachment limit',
        description: 'Future downloads only.'
      },
      {
        ...setting('activity.batch_size', 500),
        group: 'activity',
        label: 'Daemon activity batch',
        description: 'Bounded batch.',
        validation: { minimum: 1, maximum: 10_000 }
      }
    ] as SettingState[];
    const groups = [
      { id: 'activity', label: 'Activity from daemon', description: 'Projection controls.' },
      { id: 'attachments', label: 'Attachment downloads', description: 'Future downloads only.' }
    ];

    const grouped = groupSettings(settings, groups);
    expect(grouped.map((group) => group.id)).toEqual(['activity', 'attachments']);
    expect(grouped.map((group) => group.label)).toEqual(['Activity from daemon', 'Attachment downloads']);
    expect(grouped[0]?.settings[0]?.label).toBe('Daemon activity batch');
    expect(grouped[1]?.settings[0]?.key).toBe('beeper.max_media_mb');
  });
});

function setting(key: string, value: unknown): SettingState {
  return {
    key,
    group: 'browser',
    label: key,
    description: `Test fixture for ${key}.`,
    kind: typeof value === 'boolean' ? 'boolean' : 'string',
    value: typeof value === 'boolean' ? { boolean: value } : { string: String(value) },
    restart_required: true
  };
}
