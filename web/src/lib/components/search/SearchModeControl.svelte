<script lang="ts">
  import { SegmentedControl, type SegmentedControlOption } from '@kenn-io/kit-ui';

  import type { ExploreSearchMode } from '../../explore/models';
  import type { SearchCoverageStatus } from '../../search/modes';

  interface Props {
    requestedMode: ExploreSearchMode;
    status?: SearchCoverageStatus;
    error?: string;
    onchange?: (mode: ExploreSearchMode) => void;
  }

  let { requestedMode, status: _status = undefined, error: _error = '', onchange = undefined }: Props = $props();

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
