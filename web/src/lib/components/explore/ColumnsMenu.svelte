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
    if (shown && columns.length === 1) return;
    onchange(EXPLORE_COLUMNS.map(({ id }) => id).filter((id) => (id === column ? !shown : columns.includes(id))));
  }

  const sections = $derived([{
    items: EXPLORE_COLUMNS.map(({ id, label }) => ({
      id,
      label,
      active: columns.includes(id),
      closeOnSelect: false,
      onSelect: () => toggle(id)
    }))
  }]);
</script>

<FilterDropdown label="Columns" icon="more" showBadge={false} {sections} />
