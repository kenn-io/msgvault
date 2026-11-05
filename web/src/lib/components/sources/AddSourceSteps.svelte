<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';
  import { addSourceCommand, type CommandSourceKind } from '../../sources/add-source';
  import CommandLine from './CommandLine.svelte';

  let { kind, onback }: { kind: CommandSourceKind; onback: () => void } = $props();
  let values = $state<Record<string, string>>({});
  const posix = $derived(addSourceCommand(kind, values, 'posix'));
  const cmd = $derived(addSourceCommand(kind, values, 'cmd'));
</script>

<div class="steps">
  <div class="heading"><Button size="sm" surface="soft" label="Back" onclick={onback} /><h3>{kind.name}</h3></div>
  {#each kind.fields as field, index (field.id)}
    <label>{field.label}<TextInput block bind:value={() => values[field.id] ?? '', value => (values[field.id] = value)} placeholder={field.placeholder} autofocus={index === 0} /></label>
  {/each}
  {#if kind.id === 'imap'}<p>You'll be prompted for your password.</p>{/if}
  {#if posix && cmd}
    <div class="terminal"><p>POSIX shell</p><CommandLine command={posix} /></div>
    <div class="terminal">
      <p>Command Prompt</p>
      <p>Start Command Prompt with <code>cmd /d /v:off</code>, then paste:</p>
      <CommandLine command={cmd} />
    </div>
  {/if}
  <p>Run this in a terminal where msgvault is installed. Then return to Sources{#if kind.id !== 'mbox'} and open the source for its first sync command{/if}.</p>
  <a href={kind.docs} target="_blank" rel="noreferrer">Learn more</a>
</div>

<style>
  .steps { display: grid; gap: var(--space-4); }
  .heading { display: flex; align-items: center; gap: var(--control-gap); }
  h3 { margin: 0; color: var(--text-primary); font-size: var(--font-size-md); font-weight: var(--font-weight-medium); }
  label, .terminal { display: grid; gap: var(--control-gap); }
  label { color: var(--text-primary); font-size: var(--font-size-sm); }
  p { margin: 0; color: var(--text-secondary); font-size: var(--font-size-sm); }
  a { color: var(--link-ink); font-size: var(--font-size-sm); }
</style>
