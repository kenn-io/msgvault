<script lang="ts">
  import { FilterDropdown } from '@kenn-io/kit-ui';

  import { EXPLORE_COLUMNS } from '../../explore/labels';
  import type { ExploreColumn } from '../../explore/models';

  let {
    columns,
    onchange
  }: {
    columns: ExploreColumn[];
    onchange: (columns: ExploreColumn[]) => void;
  } = $props();

  function toggle(column: ExploreColumn): void {
    const shown = columns.includes(column);
    const next = EXPLORE_COLUMNS.map(({ id }) => id).filter((id) =>
      id === column ? !shown : columns.includes(id)
    );
    onchange(next);
  }

  const sections = $derived([{
    items: EXPLORE_COLUMNS.map(({ id, label }) => {
      const shown = columns.includes(id);
      return {
        // The kit derives the description element id from item.id, so keep it unique on the page.
        id: `column-${id}`,
        label,
        active: shown,
        description: shown ? 'Shown' : 'Hidden',
        disabled: shown && columns.length === 1,
        closeOnSelect: false,
        onSelect: () => toggle(id)
      };
    })
  }]);
</script>

<FilterDropdown label="Columns" icon="more" showBadge={false} {sections} />
