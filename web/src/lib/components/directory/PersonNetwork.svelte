<script lang="ts">
  import { Button, Checkbox, SelectDropdown } from '@kenn-io/kit-ui';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { NetworkEdge, NetworkNode } from '../../directory/models';

  interface Props {
    controller: DirectoryEntityController;
    onOpenPerson: (personID: number) => void;
    onOpenOrganization: (organizationID: number) => void;
  }

  let { controller, onOpenPerson, onOpenOrganization }: Props = $props();
  const graphMinimumWidth = 650;
  const nodeLabelOffset = 25;
  const nodeLabelLaneWidth = 160;
  let depth = $state('1');
  let includeEnded = $state(false);
  const depthOptions = [1, 2, 3].map((value) => ({ value: String(value), label: `${value} ${value === 1 ? 'hop' : 'hops'}` }));
  const nodes = $derived(controller.network?.nodes ?? []);
  const edges = $derived(controller.network?.edges ?? []);
  const nodeByID = $derived(new Map(nodes.map((node) => [node.id, node])));
  const rootNode = $derived(nodes.find((node) => node.id === `person:${controller.personID}`));
  const edgeGroups = $derived.by(() => {
    const grouped = new Map<number, NetworkEdge[]>();
    for (const edge of edges) {
      const source = nodeByID.get(edge.source_node_id);
      const target = nodeByID.get(edge.target_node_id);
      const hop = Math.max(source?.hop ?? 0, target?.hop ?? 0);
      grouped.set(hop, [...(grouped.get(hop) ?? []), edge]);
    }
    return [...grouped.entries()].sort(([left], [right]) => left - right);
  });
  const positions = $derived.by(() => {
    const byHop = new Map<number, NetworkNode[]>();
    for (const node of nodes) byHop.set(node.hop, [...(byHop.get(node.hop) ?? []), node]);
    const result = new Map<string, { x: number; y: number }>();
    for (const [hop, hopNodes] of byHop) {
      hopNodes.forEach((node, index) => result.set(node.id, {
        x: 70 + hop * 190,
        y: 45 + index * 80
      }));
    }
    return result;
  });
  const graphWidth = $derived(Math.max(graphMinimumWidth, ...nodes.map((node) => {
    const position = positions.get(node.id);
    if (!position) return 0;
    return Math.max(
      position.x + nodeRadius(node),
      position.x + nodeLabelOffset + nodeLabelLaneWidth
    );
  })));
  const graphHeight = $derived(Math.max(100, ...[...positions.values()].map((position) => position.y + 45)));

  $effect(() => {
    void controller.loadNetwork(Number(depth), includeEnded);
  });

  function openNode(node: NetworkNode): void {
    if (node.kind === 'person') onOpenPerson(node.entity_id);
    else onOpenOrganization(node.entity_id);
  }

  function nodeActionLabel(node: NetworkNode): string {
    return `Open ${node.kind} ${node.label}`;
  }

  function nodeRadius(node: NetworkNode): number {
    return node.kind === 'person' ? 19 : 16;
  }

  function retry(): void {
    void controller.loadNetwork(Number(depth), includeEnded);
  }
</script>

<section class="person-network" aria-label="Directory network">
  <header>
    <div>
      <h2>Network</h2>
      <p>Curated typed relationships and employments only. Messages and co-occurrence never create connections.</p>
    </div>
    <div class="controls">
      <label>Depth<SelectDropdown title="Network depth" value={depth} options={depthOptions} onchange={(value) => { depth = value; }} /></label>
      <Checkbox checked={includeEnded} label="Include ended connections" onchange={(checked) => { includeEnded = checked; }} />
    </div>
  </header>

  <div class="projection" aria-busy={controller.networkLoading}>
    {#if controller.network}
      <svg aria-hidden="true" width={graphWidth} viewBox={`0 0 ${graphWidth} ${graphHeight}`} preserveAspectRatio="xMinYMin meet">
        {#each edges as edge (edge.id)}
          {@const source = positions.get(edge.source_node_id)}
          {@const target = positions.get(edge.target_node_id)}
          {#if source && target}<line x1={source.x} y1={source.y} x2={target.x} y2={target.y} />{/if}
        {/each}
        {#each nodes as node (node.id)}
          {@const position = positions.get(node.id)}
          {#if position}
            <g transform={`translate(${position.x} ${position.y})`}>
              <circle r={nodeRadius(node)} />
              <text x={nodeLabelOffset} y="5">{node.label}</text>
            </g>
          {/if}
        {/each}
      </svg>
    {/if}

    {#if controller.networkLoading}<p class="loading" role="status">Loading network…</p>{/if}
  </div>

  {#if controller.errors.network}
    <div class="network-error" role="alert">
      <p>{controller.errors.network}</p>
      <Button label="Retry network" onclick={retry} />
    </div>
  {/if}

  {#if controller.network?.truncated}
    <p class="truncated">This is a bounded prefix with at most 250 nodes and 500 connections at depth {controller.network.depth}.</p>
  {/if}

  <ul class="connection-list" aria-label="Directory network connections">
    {#if edges.length === 0 && rootNode}
      <li class="root-only">
        <h3>Hop 0</h3>
        <Button size="sm" label={rootNode.label} ariaLabel={nodeActionLabel(rootNode)} onclick={() => openNode(rootNode)} />
      </li>
    {:else}
      {#each edgeGroups as [hop, hopEdges] (hop)}
        <li class="hop-group">
          <h3>Hop {hop}</h3>
          <ul>
            {#each hopEdges as edge (edge.id)}
              {@const source = nodeByID.get(edge.source_node_id)}
              {@const target = nodeByID.get(edge.target_node_id)}
              {#if source && target}
                <li class="connection">
                  <Button size="sm" label={source.label} ariaLabel={nodeActionLabel(source)} onclick={() => openNode(source)} />
                  <span>{edge.label}</span>
                  <Button size="sm" label={target.label} ariaLabel={nodeActionLabel(target)} onclick={() => openNode(target)} />
                  <small>{edge.kind === 'relationship' ? 'Typed relationship' : 'Employment'}</small>
                </li>
              {/if}
            {/each}
          </ul>
        </li>
      {/each}
    {/if}
  </ul>

  {#if controller.network && edges.length === 0}<p class="empty">No curated connections at this depth.</p>{/if}
</section>

<style>
  .person-network, header, .controls, .projection, .network-error, .connection-list, .hop-group, .hop-group > ul { display: grid; gap: var(--space-3); }
  header { grid-template-columns: minmax(0, 1fr) auto; align-items: start; }
  header p, h2, h3, p, ul { margin: 0; }
  header p, .empty, small { color: var(--text-muted); font-size: var(--font-size-sm); }
  .controls { grid-auto-flow: column; align-items: end; }
  .controls label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  .projection { position: relative; min-height: 7rem; overflow: auto; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  svg { min-width: 100%; max-height: 24rem; }
  line { stroke: var(--border-strong); stroke-width: 1.5; }
  circle { fill: var(--bg-surface); stroke: var(--accent-blue); stroke-width: 2; }
  text { fill: var(--text-primary); font: var(--font-size-sm) var(--font-family); }
  .loading { position: absolute; inset: 0; display: grid; place-items: center; background: color-mix(in srgb, var(--bg-surface) 72%, transparent); }
  .network-error { justify-items: start; padding: var(--space-3); border-radius: var(--radius-sm); background: var(--bg-inset); color: var(--text-danger); }
  .truncated { padding: var(--space-3); border-radius: var(--radius-sm); background: var(--bg-warning); color: var(--text-primary); }
  .connection-list, .hop-group > ul { list-style: none; padding: 0; }
  .hop-group, .root-only { padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-sm); }
  .connection { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .connection small { margin-left: auto; }
  @media (max-width: 640px) { header { grid-template-columns: 1fr; } .controls { grid-auto-flow: row; justify-items: start; } }
  @media (prefers-reduced-motion: reduce) { .projection { scroll-behavior: auto; } }
</style>
