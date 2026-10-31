import { describe, expect, it } from 'vitest';
import { NAVIGATION_GROUPS, workspaceLabel } from '../components/shell/navigation';
import { defaultExploreURLState, parseExploreURLState, serializeExploreURLState } from '../explore/state.svelte';

describe('Timeline navigation', () => {
  it('is reachable in the sidebar and survives URL serialization', () => {
    expect(NAVIGATION_GROUPS.flatMap((group) => group.items).find((item) => item.id === 'timeline')?.label).toBe('Timeline');
    expect(workspaceLabel('timeline')).toBe('Timeline');
    const url = serializeExploreURLState({ ...defaultExploreURLState, workspace: 'timeline' });
    expect(parseExploreURLState(url).workspace).toBe('timeline');
    expect(parseExploreURLState('?workspace=timeline').workspace).toBe('timeline');
  });
});
