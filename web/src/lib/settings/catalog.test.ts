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
    expect(settingsCatalog['vector.embeddings.endpoint'].testable).toBe(true);
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
});

function setting(key: string, value: unknown): SettingState {
  return {
    key,
    group: 'browser',
    kind: typeof value === 'boolean' ? 'boolean' : 'string',
    value: typeof value === 'boolean' ? { boolean: value } : { string: String(value) },
    restart_required: true
  };
}
