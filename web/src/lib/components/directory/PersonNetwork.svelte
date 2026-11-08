<script lang="ts">
  import { Button, Checkbox, SegmentedControl } from '@kenn-io/kit-ui';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { NetworkEdge, NetworkNode } from '../../directory/models';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';

  interface Props {
    controller: DirectoryEntityController;
    onOpenPerson: (personID: number) => void;
    onOpenOrganization: (organizationID: number) => void;
  }

  let { controller, onOpenPerson, onOpenOrganization }: Props = $props();
  const COLUMN = 280;
  const NAME = 170;
  const ROW = 56;
  const AVATAR = 28;
  const GAP = 8;
  let depth = $state('1');
  let includeEnded = $state(false);
  const depthOptions = [
    { value: '1', label: 'Direct' },
    { value: '2', label: '2 steps' },
    { value: '3', label: '3 steps' }
  ];
  const nodes = $derived(controller.network?.nodes ?? []);
  const edges = $derived(controller.network?.edges ?? []);
  const nodeByID = $derived(new Map(nodes.map((node) => [node.id, node])));

  $effect(() => {
    void controller.loadNetwork(Number(depth), includeEnded);
  });

  // One column per step away, each ordered by the people it hangs off so lines cross as little as possible.
  const layout = $derived.by(() => {
    const neighbours = new Map<string, string[]>();
    for (const edge of edges) {
      neighbours.set(edge.source_node_id, [...(neighbours.get(edge.source_node_id) ?? []), edge.target_node_id]);
      neighbours.set(edge.target_node_id, [...(neighbours.get(edge.target_node_id) ?? []), edge.source_node_id]);
    }
    const columns: NetworkNode[][] = [];
    for (const node of nodes) (columns[node.hop] ??= []).push(node);
    const columnRows = Array.from(columns, (column) => column?.length ?? 0);
    const tallest = Math.max(1, ...columnRows);
    const row = new Map<string, number>();
    columns.forEach((column, hop) => {
      if (hop > 0) {
        const anchor = (node: NetworkNode) => {
          const parents = (neighbours.get(node.id) ?? []).map((id) => row.get(id)).filter((value) => value !== undefined);
          return parents.length ? parents.reduce((sum, value) => sum + value, 0) / parents.length : Infinity;
        };
        column.sort((left, right) => anchor(left) - anchor(right) || left.label.localeCompare(right.label));
      }
      const offset = (tallest - columnRows[hop]!) / 2;
      column.forEach((node, index) => row.set(node.id, offset + index));
    });
    const place = new Map<string, { x: number; y: number }>();
    for (const node of nodes) place.set(node.id, { x: node.hop * COLUMN + AVATAR / 2, y: (row.get(node.id) ?? 0) * ROW + AVATAR / 2 });
    return { place, width: columns.length * COLUMN, height: (tallest - 1) * ROW + AVATAR };
  });

  // Each node's drawn width, so a line starts where its name ends.
  let widths = $state<Record<string, number>>({});

  function pairKey(edge: NetworkEdge): string {
    return JSON.stringify([edge.source_node_id, edge.target_node_id].sort());
  }

  function readingLabel(edge: NetworkEdge): string {
    const from = layout.place.get(edge.source_node_id)!;
    const to = layout.place.get(edge.target_node_id)!;
    const flipped = from.x > to.x || (from.x === to.x && from.y > to.y);
    const type = controller.relationshipTypes.find(({ slug }) => slug === edge.relationship_type_slug);
    if (type) return flipped ? type.reverse_label : type.forward_label;
    return flipped && edge.relationship_type_slug ? (from.x === to.x ? '↑ ' : '← ') + edge.label : edge.label;
  }

  const labelGroups = $derived.by(() => {
    const grouped = new Map<string, { key: string; edge: NetworkEdge; labels: string[] }>();
    for (const edge of edges) {
      const key = pairKey(edge);
      const group = grouped.get(key) ?? { key, edge, labels: [] };
      group.labels.push(readingLabel(edge));
      grouped.set(key, group);
    }
    return [...grouped.values()];
  });

  // Overlapping same-column spans use separate gutter lanes.
  const lanes = $derived.by(() => {
    const spans = labelGroups.flatMap(({ key, edge }) => {
      const a = layout.place.get(edge.source_node_id)!;
      const b = layout.place.get(edge.target_node_id)!;
      return a.x === b.x ? [{ key, x: a.x, first: Math.min(a.y, b.y), last: Math.max(a.y, b.y) }] : [];
    }).sort((a, b) => a.first - b.first);
    const ends = new Map<number, number[]>();
    const result = new Map<string, number>();
    for (const span of spans) {
      const column = ends.get(span.x) ?? [];
      let lane = column.findIndex((last) => last < span.first);
      if (lane === -1) lane = column.length;
      column[lane] = span.last;
      ends.set(span.x, column);
      result.set(span.key, span.x - AVATAR / 2 - GAP - GAP * lane);
    }
    return result;
  });

  function segment(edge: NetworkEdge) {
    const a = layout.place.get(edge.source_node_id)!;
    const b = layout.place.get(edge.target_node_id)!;
    if (a.x === b.x) {
      const [top, bottom] = a.y <= b.y ? [a, b] : [b, a];
      const x = lanes.get(pairKey(edge))!;
      const end = a.x - AVATAR / 2 - GAP;
      return { d: `M ${end} ${top.y} H ${x} V ${bottom.y} H ${end}`, tx: x, ty: (top.y + bottom.y) / 2, maxWidth: 2 * (a.x + AVATAR / 2 + GAP - x) };
    }
    const [left, right, leftID] = a.x < b.x ? [a, b, edge.source_node_id] : [b, a, edge.target_node_id];
    const x1 = left.x - AVATAR / 2 + Math.max(AVATAR, widths[leftID] ?? AVATAR) + GAP;
    const x2 = right.x - AVATAR / 2 - GAP;
    return { d: `M ${x1} ${left.y} L ${x2} ${right.y}`, tx: (x1 + x2) / 2, ty: (left.y + right.y) / 2, maxWidth: Math.min(160, x2 - x1 - GAP) };
  }

  function retry(): void {
    void controller.loadNetwork(Number(depth), includeEnded);
  }
</script>

{#if edges.length > 0 || depth !== '1' || controller.errors.network}
  <section class="person-network" aria-label="Directory network">
    <header>
      <h3>Network</h3>
      <SegmentedControl ariaLabel="Network depth" value={depth} options={depthOptions} onchange={(value) => { depth = value; }} />
      <Checkbox label="Show ended" bind:checked={includeEnded} />
    </header>
    {#if controller.errors.network}
      <div class="refresh-error" role="alert">
        <p>{controller.errors.network}</p>
        <Button label="Retry network" size="sm" onclick={retry} />
      </div>
    {/if}
    <!-- Keyboard focus lets a user scroll a graph wider than its pane without a pointer. -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div class="scroll" tabindex="0" role="region" aria-label="Network graph" aria-busy={controller.networkLoading}>
      <div class="graph" style:width={`${layout.width}px`} style:height={`${layout.height}px`}>
        <svg width={layout.width} height={layout.height} aria-hidden="true">
          {#each edges as edge (edge.id)}
            {@const line = segment(edge)}
            <path d={line.d} />
          {/each}
        </svg>
        {#each labelGroups as group (group.key)}
          {@const line = segment(group.edge)}
          <span class="edge-label" aria-hidden="true" style:left={`${line.tx}px`} style:top={`${line.ty}px`} style:max-width={`${line.maxWidth}px`} title={group.labels.join(' · ')}>{group.labels.join(' · ')}</span>
        {/each}
        {#each nodes as node (node.id)}
          {@const at = layout.place.get(node.id)}
          {#if at}
            <div class="node" class:node--root={node.hop === 0} style:left={`${at.x - AVATAR / 2}px`} style:top={`${at.y - AVATAR / 2}px`} style:max-width={`${NAME + AVATAR}px`} title={node.label} bind:clientWidth={widths[node.id]}>
              <IdentityAvatar label={node.label} seed={node.id} shape={node.kind === 'person' ? 'person' : 'domain'} size={AVATAR} />
              {#if node.hop === 0}<span class="name">{node.label}</span>
              {:else}
                <!-- kit-ui-check-ignore: a record name styled as a link, matching Connections and Work. -->
                <button type="button" class="name link" onclick={() => node.kind === 'person' ? onOpenPerson(node.entity_id) : onOpenOrganization(node.entity_id)}>{node.label}</button>
              {/if}
            </div>
          {/if}
        {/each}
      </div>
      {#if controller.networkLoading}<p class="loading" role="status">Loading network…</p>{/if}
    </div>
    {#if controller.network?.truncated}<p class="truncated">Showing up to 250 people and organizations and 500 connections.</p>{/if}
    <ul class="kit-sr-only" aria-label="Network connections">
      {#each edges as edge (edge.id)}
        <li>{nodeByID.get(edge.source_node_id)?.label} · {edge.label} · {nodeByID.get(edge.target_node_id)?.label}</li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .person-network { display: grid; gap: var(--space-2); }
  header { display: flex; align-items: center; justify-content: space-between; min-height: var(--control-h); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .scroll { position: relative; overflow-x: auto; padding-top: var(--space-2); }
  .scroll:focus-visible { outline: var(--focus-ring); outline-offset: var(--focus-ring-offset, 2px); }
  .graph { position: relative; }
  svg { position: absolute; inset: 0; overflow: visible; }
  path { fill: none; stroke: color-mix(in srgb, var(--text-muted) 55%, transparent); stroke-width: 1; }
  .node { position: absolute; display: flex; align-items: center; gap: var(--space-2); }
  .name { position: relative; z-index: 2; max-width: 170px; overflow: hidden; background: var(--bg-surface); font-size: var(--font-size-sm); text-overflow: ellipsis; white-space: nowrap; }
  .node--root .name { font-weight: var(--font-weight-semibold, 600); }
  /* kit-ui-check-ignore: a text link inside the graph, matching record links in Connections and Work. */
  .link { padding: 0; border: 0; color: var(--link-ink); cursor: pointer; font: inherit; font-size: var(--font-size-sm); }
  .link:hover { text-decoration: underline; }
  .edge-label { position: absolute; z-index: 1; transform: translate(-50%, -50%); box-sizing: border-box; overflow: hidden; text-overflow: ellipsis; padding: 0 var(--chip-pad-x); border: 1px solid var(--border-strong); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-muted); font-size: var(--font-size-xs); line-height: 18px; white-space: nowrap; }
  .loading { position: absolute; inset: 0; display: grid; place-items: center; background: color-mix(in srgb, var(--bg-surface) 72%, transparent); font-size: var(--font-size-sm); color: var(--text-muted); }
  .refresh-error { display: flex; align-items: center; gap: var(--space-2); color: var(--text-danger); font-size: var(--font-size-sm); }
  .truncated { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
