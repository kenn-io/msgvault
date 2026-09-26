<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { Checkbox, copyToClipboard } from '@kenn-io/kit-ui';
  import {
    PeopleInferenceController,
    type PeopleInferencePort,
    type PeopleProvider,
  } from '../../settings/people-inference-controller.svelte';

  let { port }: { port: PeopleInferencePort } = $props();
  const controller = new PeopleInferenceController(untrack(() => port));
  let provider = $state<PeopleProvider>('openai');
  let name = $state('');
  let model = $state('');
  let key = $state('');
  let replacementKey = $state('');
  let credentialMode = $state<'key' | 'environment'>('key');
  let environmentVariable = $state('');
  let allowedSources = $state<string[]>([]);
  let sourceSince = $state('');
  let sourceUntil = $state('');
  let sensitiveChoice = $state<'' | 'allow' | 'exclude'>('');
  let retentionPosture = $state('');
  let trainingPosture = $state('');
  let removeConfirmationName = $state('');
  const datePattern = '[0-9]{4}-[0-9]{2}-[0-9]{2}';

  onMount(() => { void controller.load(); });
  onDestroy(() => controller.destroy());

  async function createProfile(): Promise<void> {
    const stableName = name.trim();
    if (provider !== 'codex' && credentialMode === 'environment' &&
      !/^[A-Za-z_][A-Za-z0-9_]*$/.test(environmentVariable.trim())) {
      controller.error = 'Enter a valid environment variable name on the daemon host.';
      return;
    }
    if (!stableName || (provider !== 'codex' && !model.trim()) ||
      (provider === 'codex' && (controller.loginState !== 'complete' ||
        !controller.selectedModel || !controller.reasoningEffort)) || allowedSources.length === 0 ||
      !sourceSince || !retentionPosture.trim() || !trainingPosture.trim() || !sensitiveChoice) {
      controller.error = 'Complete the model and archive disclosure policy before creating a profile.';
      return;
    }
    await controller.create({
      name: stableName,
      provider,
      model: provider === 'codex' ? controller.selectedModel : model.trim(),
      ...(provider === 'codex' ? { reasoningEffort: controller.reasoningEffort } : {}),
      environmentVariable: provider !== 'codex' && credentialMode === 'environment' ? environmentVariable.trim() : undefined,
      allowedSources,
      sourceSince,
      sourceUntil: sourceUntil || undefined,
      allowSensitive: sensitiveChoice === 'allow',
      retentionPosture: retentionPosture.trim(),
      trainingPosture: trainingPosture.trim(),
    }, provider !== 'codex' && credentialMode === 'key' ? key : '');
    key = '';
    if (!controller.error) {
      name = '';
      model = '';
      environmentVariable = '';
      allowedSources = [];
      sourceSince = '';
      sourceUntil = '';
      sensitiveChoice = '';
      retentionPosture = '';
      trainingPosture = '';
    }
  }

  async function copy(value: string): Promise<void> {
    if (!await copyToClipboard(value)) {
      controller.error = 'Could not copy. Select the text and copy it manually.';
    }
  }

  async function confirmRemoval(): Promise<void> {
    const target = removeConfirmationName;
    removeConfirmationName = '';
    if (target) await controller.remove(target);
  }

  async function saveReplacementKey(): Promise<void> {
    await controller.saveKey(replacementKey);
    if (!controller.error) replacementKey = '';
  }

  function setSource(source: string, checked: boolean): void {
    allowedSources = checked ? [...allowedSources, source] : allowedSources.filter((item) => item !== source);
  }

  function startDraftLogin(): void {
    const stableName = name.trim();
    if (!/^[A-Za-z0-9._:-]+$/.test(stableName)) {
      controller.error = 'Enter a valid profile name before signing in.';
      return;
    }
    if (controller.status?.profiles.some((profile) => profile.name === stableName)) {
      controller.error = 'A profile with this name already exists.';
      return;
    }
    void controller.startLogin(stableName);
  }
</script>

<section class="people-inference" aria-label="People sweep settings">
  <header>
    <h2>People sweep</h2>
    <p>Choose the provider used for people sweep and briefs. A synthetic check and your consent are required before use.</p>
    <button type="button" disabled={controller.loading || controller.busy || Boolean(controller.login && controller.loginState !== 'complete')} onclick={() => void controller.load()}>Reload people sweep settings</button>
  </header>

  {#if controller.loading}
    <p role="status">Loading people sweep settings…</p>
  {:else if controller.status?.unavailableReason}
    <p role="status">{controller.status.unavailableReason}</p>
  {:else if !controller.status}
    <p role="alert">{controller.error || 'Unable to load people sweep settings.'}</p>
  {:else}
    {#if controller.error}<p role="alert" class="error">{controller.error}</p>{/if}
    {#if controller.status?.pendingRestart}
      <p role="status" class="restart">Restart the daemon to use the saved people sweep profile. The running daemon still uses {controller.status.runningName || 'no profile'}.</p>
    {/if}

    <div class="status">
      <p><strong>Configured:</strong> {controller.status?.configuredName || 'None'} ({controller.status?.configuredEnabled ? 'enabled' : 'disabled'})</p>
      <p><strong>Running:</strong> {controller.status?.runningName || 'None'} ({controller.status?.runningEnabled ? 'enabled' : 'disabled'})</p>
      {#if controller.status?.configuredEnabled}
        <button type="button" disabled={controller.busy} onclick={() => void controller.disable()}>Disable people sweep</button>
      {/if}
    </div>

    <form class="form" onsubmit={(event) => { event.preventDefault(); void createProfile(); }}>
      <h3>Add a profile</h3>
      <label>Provider
        <select bind:value={provider} disabled={controller.busy || (provider === 'codex' && Boolean(controller.login))} onchange={() => {
          key = '';
          environmentVariable = '';
          credentialMode = 'key';
        }}>
          <option value="openai">OpenAI Platform</option>
          <option value="openrouter">OpenRouter</option>
          <option value="venice">Venice</option>
          {#if controller.supportsCodex}<option value="codex">Codex subscription</option>{/if}
        </select>
      </label>
      <label>Profile name
        <input bind:value={name} required pattern="[A-Za-z0-9._:-]+" autocomplete="off" disabled={controller.busy || (provider === 'codex' && Boolean(controller.login))} />
      </label>
      {#if provider === 'codex'}
        <div class="login" aria-label="Codex profile sign-in">
          <button type="button" disabled={controller.busy || Boolean(controller.login)} onclick={startDraftLogin}>Sign in with Codex</button>
          {#if controller.login}
            <p><strong>Open:</strong> <span class="copyable">{controller.login.url}</span> <button type="button" aria-label="Copy sign-in URL" onclick={() => void copy(controller.login!.url)}>Copy URL</button></p>
            <p><strong>Enter code:</strong> <span class="copyable">{controller.login.code}</span> <button type="button" aria-label="Copy sign-in code" onclick={() => void copy(controller.login!.code)}>Copy code</button></p>
            <p>Local sign-in session ends {new Date(controller.login.localDeadline).toLocaleString()}.</p>
            {#if controller.loginState === 'pending'}<p role="status">Waiting for Codex sign-in…</p>{/if}
            {#if controller.loginState === 'complete'}<p role="status">Signed in. Choose a model and reasoning effort.</p>{/if}
            <button type="button" onclick={() => void controller.cancelLogin()}>Cancel sign-in</button>
          {/if}
          {#if controller.availableModels.length}
            <label>Model
              <select bind:value={controller.selectedModel} onchange={() => {
                controller.reasoningEffort = controller.availableModels.find((item) => item.id === controller.selectedModel)?.reasoningEfforts[0] ?? '';
              }}>
                {#each controller.availableModels as item (item.id)}<option value={item.id}>{item.id}</option>{/each}
              </select>
            </label>
            <label>Reasoning effort
              <select bind:value={controller.reasoningEffort}>
                {#each controller.availableModels.find((item) => item.id === controller.selectedModel)?.reasoningEfforts ?? [] as effort (effort)}
                  <option value={effort}>{effort}</option>
                {/each}
              </select>
            </label>
          {/if}
        </div>
      {/if}
      {#if provider !== 'codex'}
        <label>Model ID
          <input bind:value={model} required autocomplete="off" />
        </label>
        {#if port.supportsEnvironmentReference}
          <fieldset>
            <legend>Credential source</legend>
            <label><input type="radio" value="key" bind:group={credentialMode} /> Store an API key</label>
            <label><input type="radio" value="environment" bind:group={credentialMode} /> Environment variable on daemon host</label>
          </fieldset>
        {/if}
        {#if credentialMode === 'key'}
          <label>API key
            <input type="password" bind:value={key} autocomplete="new-password" required />
          </label>
        {:else}
          <label>Environment variable
            <input bind:value={environmentVariable} pattern="[A-Za-z_][A-Za-z0-9_]*" autocomplete="off" required />
          </label>
        {/if}
      {/if}
      <fieldset>
        <legend>Archive source classes</legend>
        <Checkbox label="Conversation text" checked={allowedSources.includes('conversation_text')} onchange={(checked) => setSource('conversation_text', checked)} />
        <Checkbox label="Meeting text" checked={allowedSources.includes('meeting_text')} onchange={(checked) => setSource('meeting_text', checked)} />
        <Checkbox label="Document text" checked={allowedSources.includes('document_text')} onchange={(checked) => setSource('document_text', checked)} />
      </fieldset>
      <label>Archive data since (YYYY-MM-DD)
        <input type="text" bind:value={sourceSince} required pattern={datePattern} placeholder="YYYY-MM-DD" autocomplete="off" />
      </label>
      <label>Archive data until (optional, YYYY-MM-DD)
        <input type="text" bind:value={sourceUntil} pattern={datePattern} placeholder="YYYY-MM-DD" autocomplete="off" />
      </label>
      <fieldset>
        <legend>Sensitive archive content</legend>
        <label><input type="radio" name="sensitive-content" value="allow" bind:group={sensitiveChoice} required /> Allow sensitive content</label>
        <label><input type="radio" name="sensitive-content" value="exclude" bind:group={sensitiveChoice} /> Exclude sensitive content</label>
        <p>Real sweeps send archive text to this provider. Excluding sensitive content permits only the synthetic check.</p>
      </fieldset>
      <label>Retention statement
        <input bind:value={retentionPosture} required autocomplete="off" />
      </label>
      <label>Training statement
        <input bind:value={trainingPosture} required autocomplete="off" />
      </label>
      {#if provider === 'codex'}
        <button type="submit" disabled={controller.busy || controller.loginState !== 'complete' || !controller.selectedModel || !controller.reasoningEffort}>Create Codex profile</button>
      {:else}
        <button type="submit" disabled={controller.busy}>Create profile</button>
      {/if}
    </form>

    {#if controller.status?.profiles.length}
      <div class="profile">
        <h3>Profile setup</h3>
        <label>Profile
          <select value={controller.selectedName} onchange={(event) => {
            removeConfirmationName = '';
            replacementKey = '';
            controller.choose(event.currentTarget.value);
          }}>
            {#each controller.status.profiles as profile (profile.name)}
              <option value={profile.name}>{profile.name}</option>
            {/each}
          </select>
        </label>
        {#if controller.selectedProfile}
          <dl>
            <div><dt>Provider</dt><dd>{controller.selectedProfile.provider}</dd></div>
            <div><dt>Model</dt><dd>{controller.selectedProfile.model || 'Choose after sign-in'}</dd></div>
            <div><dt>Credential</dt><dd>{controller.selectedProfile.credentialStatus}</dd></div>
            <div><dt>Last check</dt><dd>{controller.selectedProfile.lastCheck || 'Not checked'}</dd></div>
            <div><dt>Consent</dt><dd>{controller.selectedProfile.consentedFingerprint === controller.selectedProfile.fingerprint ? 'Granted for this profile' : 'Not granted'}</dd></div>
          </dl>
          {#if controller.selectedProfile.provider !== 'codex' && controller.selectedProfile.credentialSource !== 'env'}
            <form class="replacement-key" onsubmit={(event) => { event.preventDefault(); void saveReplacementKey(); }}>
              <label>Replacement API key
                <input type="password" bind:value={replacementKey} autocomplete="new-password" required />
              </label>
              <button type="submit" disabled={controller.busy}>Save API key</button>
            </form>
          {/if}
          {#if controller.selectedProfile.provider === 'codex' && controller.supportsCodex && (!controller.selectedProfile.model || controller.selectedProfile.credentialConfigured === false)}
            <div class="login">
              <button type="button" disabled={controller.busy || Boolean(controller.login)} onclick={() => void controller.startLogin()}>Sign in with Codex</button>
              {#if controller.login}
                <p><strong>Open:</strong> <span class="copyable">{controller.login.url}</span> <button type="button" aria-label="Copy sign-in URL" onclick={() => void copy(controller.login!.url)}>Copy URL</button></p>
                <p><strong>Enter code:</strong> <span class="copyable">{controller.login.code}</span> <button type="button" aria-label="Copy sign-in code" onclick={() => void copy(controller.login!.code)}>Copy code</button></p>
                <p>Local sign-in session ends {new Date(controller.login.localDeadline).toLocaleString()}.</p>
                {#if controller.loginState === 'pending'}
                  <p role="status">Waiting for Codex sign-in…</p>
                  <button type="button" onclick={() => void controller.cancelLogin()}>Cancel sign-in</button>
                {:else if controller.loginState === 'complete'}
                  <p role="status">Signed in. Choose a model and run a synthetic check.</p>
                {/if}
              {/if}
              {#if controller.availableModels.length}
                <label>Model
                  <select bind:value={controller.selectedModel} onchange={() => {
                    controller.reasoningEffort = controller.availableModels.find((item) => item.id === controller.selectedModel)?.reasoningEfforts[0] ?? '';
                  }}>
                    {#each controller.availableModels as item (item.id)}<option value={item.id}>{item.id}</option>{/each}
                  </select>
                </label>
                <label>Reasoning effort
                  <select bind:value={controller.reasoningEffort}>
                    {#each controller.availableModels.find((item) => item.id === controller.selectedModel)?.reasoningEfforts ?? [] as effort (effort)}
                      <option value={effort}>{effort}</option>
                    {/each}
                  </select>
                </label>
              {/if}
            </div>
          {/if}

          <div class="actions">
            <button type="button" disabled={controller.busy || (controller.selectedProfile.credentialSource === 'env' && !controller.selectedProfile.credentialConfigured) || (controller.selectedProfile.provider === 'codex' && (controller.selectedProfile.credentialConfigured === false || (!controller.selectedProfile.model && controller.loginState !== 'complete')))} onclick={() => void controller.check()}>Check provider</button>
            <button type="button" disabled={controller.busy || !controller.canSelect} onclick={() => void controller.select()}>Select and enable</button>
            {#if controller.selectedProfile.fingerprint && controller.selectedProfile.consentedFingerprint === controller.selectedProfile.fingerprint}
              <button type="button" disabled={controller.busy} onclick={() => void controller.revoke()}>Revoke consent</button>
            {/if}
            {#if controller.status.profiles.length > 1 && removeConfirmationName !== controller.selectedName}
              <button type="button" disabled={controller.busy || (controller.status.configuredEnabled && controller.status.configuredName === controller.selectedName)} onclick={() => removeConfirmationName = controller.selectedName}>Remove profile</button>
            {/if}
          </div>
          {#if removeConfirmationName === controller.selectedName}
            <div class="remove-confirmation">
              <p>Removing {removeConfirmationName} removes its stored credential and saved policy. This cannot be undone.</p>
              <button type="button" disabled={controller.busy} onclick={() => void confirmRemoval()}>Confirm removal</button>
              <button type="button" disabled={controller.busy} onclick={() => removeConfirmationName = ''}>Cancel removal</button>
            </div>
          {/if}
          {#if controller.checkResult}
            {@const disclosure = controller.checkResult.disclosure}
            <section class="disclosure" aria-label="Archive disclosure">
              <h4>Review archive disclosure</h4>
              <p>Source classes: {disclosure.sourceClasses.join(', ') || 'None'}</p>
              <p>Date bounds: {disclosure.since || 'Unbounded'} to {disclosure.until || 'Unbounded'}</p>
              <p>Sensitive content: {disclosure.sensitiveContent ? 'Allowed' : 'Excluded'}</p>
              <p>Provider endpoint: {disclosure.endpoint}</p>
              <p>Retention: {disclosure.retention}</p>
              <p>Training: {disclosure.training}</p>
              {#if controller.selectedProfile?.fingerprint !== controller.checkResult.fingerprint}
                <p role="alert">This profile changed after the check. Run a new synthetic check before granting consent.</p>
              {/if}
              <Checkbox bind:checked={controller.disclosureConfirmed} label="I confirm this exact disclosure" />
              <button type="button" disabled={controller.busy || !controller.canConsent} onclick={() => void controller.consent()}>Grant consent</button>
            </section>
          {/if}
        {/if}
      </div>
    {/if}
  {/if}
</section>

<style>
  .people-inference { display: grid; gap: var(--space-5); min-width: 0; }
  header p { color: var(--text-muted); margin-block: var(--space-2) 0; }
  .form, .profile, .login, .disclosure, .replacement-key { display: grid; gap: var(--space-3); min-width: 0; }
  .disclosure { grid-template-columns: minmax(0, 1fr); overflow-wrap: anywhere; }
  .form, .profile { border: 1px solid var(--border-muted); border-radius: var(--radius-md); padding: var(--space-4); }
  label { display: grid; gap: var(--space-1); min-width: 0; }
  input:not([type='radio'], [type='checkbox']), select { width: 100%; min-width: 0; }
  fieldset { border: 0; padding: 0; display: grid; gap: var(--space-2); }
  fieldset label { display: flex; align-items: center; gap: var(--space-2); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .remove-confirmation { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  .remove-confirmation p { flex-basis: 100%; margin: 0; }
  dl { display: grid; gap: var(--space-2); margin: 0; }
  dl div { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  dt { color: var(--text-muted); }
  dd { margin: 0; overflow-wrap: anywhere; }
  .copyable { overflow-wrap: anywhere; }
  .error { color: var(--text-danger); }
  .restart { border: 1px solid var(--border-warning); padding: var(--space-3); }
</style>
