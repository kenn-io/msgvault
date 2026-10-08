<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { Button, Checkbox, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { PeopleInferencePresetCreateRequest } from '../../api/generated/models';
  import { credentialStatus, PeopleInferenceController } from '../../settings/people-inference-controller.svelte';

  let { client }: { client: APIClient } = $props();
  const controller = new PeopleInferenceController(untrack(() => client));
  let provider = $state<PeopleInferencePresetCreateRequest['preset_id']>('openai');
  let name = $state('');
  let model = $state('');
  let key = $state('');
  let replacementKey = $state('');
  let allowedSources = $state<string[]>([]);
  let sourceSince = $state('');
  let sourceUntil = $state('');
  let sensitiveChoice = $state('');
  let retentionPosture = $state('');
  let trainingPosture = $state('');
  let removeConfirmationName = $state('');

  onMount(() => { void controller.load(); });
  onDestroy(() => controller.destroy());

  async function createProfile(): Promise<void> {
    const stableName = name.trim();
    if (!/^[A-Za-z0-9._:-]+$/.test(stableName)) {
      controller.error = 'Enter a valid profile name using letters, digits, dots, underscores, colons, or hyphens.';
      return;
    }
    if (!model.trim() || !key || allowedSources.length === 0 || !sourceSince ||
      !retentionPosture.trim() || !trainingPosture.trim() || !sensitiveChoice) {
      controller.error = 'Complete the model, key, and archive disclosure policy before creating a profile.';
      return;
    }
    await controller.create(stableName, {
      preset_id: provider, model: model.trim(), allowed_sources: allowedSources,
      source_since: sourceSince, source_until: sourceUntil || undefined,
      allow_sensitive: sensitiveChoice === 'allow',
      retention_posture: retentionPosture.trim(), training_posture: trainingPosture.trim(),
    }, key);
    key = '';
    if (controller.status?.profiles.some((profile) => profile.name === stableName)) {
      name = '';
      model = '';
      allowedSources = [];
      sourceSince = '';
      sourceUntil = '';
      sensitiveChoice = '';
      retentionPosture = '';
      trainingPosture = '';
    }
  }

  async function saveReplacementKey(): Promise<void> {
    await controller.saveKey(replacementKey);
    replacementKey = '';
  }

  function setSource(source: string, checked: boolean): void {
    allowedSources = checked ? [...allowedSources, source] : allowedSources.filter((item) => item !== source);
  }
</script>

<section class="people-inference" aria-label="People sweep settings">
  <header>
    <h2>People sweep</h2>
    <p>Choose the provider used for people sweep and briefs. A synthetic check and your consent are required before use.</p>
    <Button disabled={controller.loading || controller.busy} onclick={() => void controller.load()} label="Reload people sweep settings" />
  </header>

  {#if controller.loading}
    <p role="status">Loading people sweep settings…</p>
  {:else if !controller.status}
    <p role="alert">{controller.error || 'Unable to load people sweep settings.'}</p>
  {:else}
    {#if controller.error}<p role="alert" class="error">{controller.error}</p>{/if}
    {#if controller.status.pending_restart}
      <p role="status">Restart the daemon to use the saved people sweep profile. The running daemon still uses {controller.status.running_name || 'no profile'}.</p>
    {/if}
    <div>
      <p><strong>Configured:</strong> {controller.status.configured_name || 'None'} ({controller.status.configured_enabled ? 'enabled' : 'disabled'})</p>
      <p><strong>Running:</strong> {controller.status.running_name || 'None'} ({controller.status.running_enabled ? 'enabled' : 'disabled'})</p>
      {#if controller.status.configured_enabled}
        <Button disabled={controller.busy} onclick={() => void controller.disable()} label="Disable people sweep" />
      {/if}
    </div>

    <form class="form" onsubmit={(event) => { event.preventDefault(); void createProfile(); }}>
      <h3>Add a profile</h3>
      <SelectDropdown title="Provider" value={provider} disabled={controller.busy}
        options={[{ value: 'openai', label: 'OpenAI Platform' }, { value: 'openrouter', label: 'OpenRouter' }, { value: 'venice', label: 'Venice' }]}
        onchange={(value) => { provider = value as typeof provider; key = ''; }} />
      <label>Profile name<TextInput bind:value={name} required autocomplete="off" disabled={controller.busy} block /></label>
      <label>Model ID<TextInput bind:value={model} required autocomplete="off" block /></label>
      <label>API key<TextInput type="password" bind:value={key} autocomplete="new-password" required block /></label>
      <fieldset>
        <legend>Archive source classes</legend>
        <Checkbox label="Conversation text" checked={allowedSources.includes('conversation_text')} onchange={(checked) => setSource('conversation_text', checked)} />
        <Checkbox label="Meeting text" checked={allowedSources.includes('meeting_text')} onchange={(checked) => setSource('meeting_text', checked)} />
        <Checkbox label="Document text" checked={allowedSources.includes('document_text')} onchange={(checked) => setSource('document_text', checked)} />
      </fieldset>
      <label>Archive data since (YYYY-MM-DD)<TextInput bind:value={sourceSince} placeholder="YYYY-MM-DD" required block /></label>
      <label>Archive data until (optional, YYYY-MM-DD)<TextInput bind:value={sourceUntil} placeholder="YYYY-MM-DD" block /></label>
      <fieldset>
        <legend>Sensitive archive content</legend>
        <label><input type="radio" name="sensitive-content" value="allow" bind:group={sensitiveChoice} required /> Allow sensitive content</label>
        <label><input type="radio" name="sensitive-content" value="exclude" bind:group={sensitiveChoice} /> Exclude sensitive content</label>
        <p>Real sweeps send archive text to this provider. Excluding sensitive content permits only the synthetic check.</p>
      </fieldset>
      <label>Retention statement<TextInput bind:value={retentionPosture} required autocomplete="off" block /></label>
      <label>Training statement<TextInput bind:value={trainingPosture} required autocomplete="off" block /></label>
      <Button type="submit" disabled={controller.busy} label="Create profile" />
    </form>

    {#if controller.status.profiles.length}
      <div class="profile">
        <h3>Profile setup</h3>
        <SelectDropdown title="Profile" value={controller.selectedName} disabled={controller.busy}
          options={controller.status.profiles.map((profile) => ({ value: profile.name, label: profile.name }))}
          onchange={(value) => { removeConfirmationName = ''; replacementKey = ''; controller.choose(value); }} />
        {#if controller.selectedProfile}
          {@const profile = controller.selectedProfile}
          <dl>
            <div><dt>Provider</dt><dd>{profile.preset_id || profile.protocol}</dd></div>
            <div><dt>Model</dt><dd>{profile.model || 'Not configured'}</dd></div>
            <div><dt>Credential</dt><dd>{credentialStatus(profile)}</dd></div>
            <div><dt>Last check</dt><dd>{profile.checked ? 'Checked for this profile' : 'Not checked'}</dd></div>
            <div><dt>Consent</dt><dd>{profile.consent_active ? 'Granted for this profile' : 'Not granted'}</dd></div>
          </dl>
          {#if profile.protocol !== 'codex_app_server' && profile.credential_source !== 'env'}
            <form class="replacement-key" onsubmit={(event) => { event.preventDefault(); void saveReplacementKey(); }}>
              <label>Replacement API key<TextInput type="password" bind:value={replacementKey} autocomplete="new-password" required block /></label>
              <Button type="submit" disabled={controller.busy} label="Save API key" />
            </form>
          {/if}
          <div class="actions">
            <Button disabled={controller.busy || !profile.credential_configured || profile.protocol === 'codex_app_server'} onclick={() => void controller.check()} label="Check provider" />
            <Button disabled={controller.busy || !controller.canSelect} onclick={() => void controller.select()} label="Select and enable" />
            {#if profile.consent_active}
              <Button disabled={controller.busy} onclick={() => void controller.revoke()} label="Revoke consent" />
            {/if}
            {#if removeConfirmationName !== controller.selectedName}
              <Button disabled={controller.busy || controller.status.profiles.length < 2 || (controller.status.configured_enabled && controller.status.configured_name === controller.selectedName)} onclick={() => removeConfirmationName = controller.selectedName} label="Remove profile" />
            {/if}
          </div>
          {#if controller.status.profiles.length < 2}
            <p>Add another profile before removing this one.</p>
          {/if}
          {#if removeConfirmationName === controller.selectedName}
            <div>
              <p>Removing {removeConfirmationName} removes its stored credential and saved policy. This cannot be undone.</p>
              <Button disabled={controller.busy || controller.status.profiles.length < 2} onclick={() => {
                const target = removeConfirmationName;
                removeConfirmationName = '';
                void controller.remove(target);
              }} label="Confirm removal" />
              <Button disabled={controller.busy} onclick={() => removeConfirmationName = ''} label="Cancel removal" />
            </div>
          {/if}
          {#if controller.checkedProfile}
            {@const disclosure = controller.checkedProfile}
            <section class="disclosure" aria-label="Archive disclosure">
              <h4>Review archive disclosure</h4>
              <p>Source classes: {disclosure.allowed_sources.join(', ') || 'None'}</p>
              <p>Date bounds: {disclosure.source_since || 'Unbounded'} to {disclosure.source_until || 'Unbounded'}</p>
              <p>Sensitive content: {disclosure.allow_sensitive ? 'Allowed' : 'Excluded'}</p>
              <p>Provider endpoint: {disclosure.endpoint}</p>
              <p>Retention: {disclosure.retention_posture}</p>
              <p>Training: {disclosure.training_posture}</p>
              <Checkbox bind:checked={controller.disclosureConfirmed} label="I confirm this exact disclosure" />
              <Button disabled={controller.busy || !controller.canConsent} onclick={() => void controller.consent()} label="Grant consent" />
            </section>
          {/if}
        {/if}
      </div>
    {/if}
  {/if}
</section>

<style>
  .people-inference { display: grid; gap: var(--space-5); min-width: 0; overflow-wrap: anywhere; }
  header p { color: var(--text-muted); margin-block: var(--space-2); }
  .form, .profile, .disclosure, .replacement-key { display: grid; gap: var(--space-3); min-width: 0; }
  .form, .profile { border: 1px solid var(--border-muted); border-radius: var(--radius-md); padding: var(--space-4); }
  label { display: grid; gap: var(--space-1); min-width: 0; }
  label:has(input[type='radio']) { display: flex; align-items: center; }
  fieldset { display: grid; gap: var(--space-2); min-width: 0; border: 1px solid var(--border-muted); }
  fieldset p { margin: 0; }
  dl { display: grid; gap: var(--space-2); margin: 0; }
  dl div { display: grid; grid-template-columns: 6rem minmax(0, 1fr); gap: var(--space-2); }
  dt { color: var(--text-muted); }
  dd { margin: 0; }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .error { color: var(--danger); }
</style>
