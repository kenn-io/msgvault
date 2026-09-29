<script lang="ts">
  import { SegmentedControl, type SegmentedControlOption } from '@kenn-io/kit-ui';

  import type { ExploreSearchMode } from '../../explore/models';

  interface Props {
    requestedMode: ExploreSearchMode;
    onchange?: (mode: ExploreSearchMode) => void;
  }

  let { requestedMode, onchange = undefined }: Props = $props();

  const options: SegmentedControlOption[] = [
    { value: 'full_text', label: 'Full text' },
    { value: 'semantic', label: 'Semantic' },
    { value: 'hybrid', label: 'Hybrid' }
  ];
</script>

<SegmentedControl
  ariaLabel="Search mode"
  {options}
  value={requestedMode}
  onchange={(mode) => onchange?.(mode as ExploreSearchMode)}
/>
