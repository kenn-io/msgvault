import { describe, expect, it } from 'vitest';

import { groupSettings, hasHostManaged, restartPosture, type SettingState } from './catalog';

describe('settings catalog', () => {
  it('arranges settings into daemon groups and sections in daemon order', () => {
    const settings = [
      setting('beeper.max_media_mb', 0, { group: 'attachments', section: 'beeper', label: 'Beeper maximum size' }),
      setting('activity.batch_size', 500, { group: 'archive', section: 'activity', label: 'Batch size' }),
      setting('analytics.engine', 'auto', { group: 'archive', section: 'analytics', label: 'Analytics engine' }),
      setting('unsupported.private_value', 'hidden', { group: 'private' as SettingState['group'] }),
    ];
    const groups = [
      {
        id: 'archive', label: 'Archive', description: 'Cache, activity, and backups.',
        sections: [
          { id: 'analytics', label: 'Analytics cache', description: 'Aggregate views.' },
          { id: 'activity', label: 'Activity history' },
          { id: 'backups', label: 'Backups' },
        ],
      },
      { id: 'attachments', label: 'Attachments', description: 'Future downloads only.' },
      { id: 'integrations', label: 'Integrations', description: 'No settings here yet.' },
    ];

    const grouped = groupSettings(settings, groups);
    expect(grouped.map((group) => group.id)).toEqual(['archive', 'attachments']);
    expect(grouped[0]?.sections.map((section) => section.id)).toEqual(['analytics', 'activity']);
    expect(grouped[0]?.sections[0]?.description).toBe('Aggregate views.');
    expect(grouped[0]?.sections[1]?.description).toBe('');
    expect(grouped[0]?.sections[1]?.settings.map((item) => item.key)).toEqual(['activity.batch_size']);
    expect(grouped[1]?.sections).toEqual([]);
    expect(grouped[1]?.settings.map((item) => item.label)).toEqual(['Beeper maximum size']);
    expect(grouped.flatMap((group) => group.settings.map((item) => item.key))).not.toContain(
      'unsupported.private_value',
    );
  });

  it('reports how a run of settings takes effect, ignoring host-managed rows', () => {
    const live = setting('web.theme', 'dark', { restart_required: false });
    const restart = setting('log.level', 'info');
    const hostManaged = setting('server.bind_addr', '127.0.0.1', { read_only: true });

    expect(restartPosture([live, live])).toBe('live');
    expect(restartPosture([restart, hostManaged])).toBe('restart');
    expect(restartPosture([live, restart])).toBe('mixed');
    expect(restartPosture([hostManaged])).toBe('none');
    expect(restartPosture([])).toBe('none');
    expect(hasHostManaged([restart, hostManaged])).toBe(true);
    expect(hasHostManaged([restart])).toBe(false);
  });
});

function setting(key: string, value: unknown, overrides: Partial<SettingState> = {}): SettingState {
  return {
    key,
    group: 'browser',
    label: key,
    description: `Test fixture for ${key}.`,
    kind: typeof value === 'boolean' ? 'boolean' : 'string',
    value: typeof value === 'boolean' ? { boolean: value } : { string: String(value) },
    restart_required: true,
    ...overrides,
  };
}
