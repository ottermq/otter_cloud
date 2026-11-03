<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import { ApiError } from '../api'
  import { link } from '../router.svelte'
  import { authed } from '../session.svelte'
  import {
    leftWords,
    words,
    type Catalog,
    type Instance,
    type Operation,
    type SubscriptionAnswer,
  } from '../subscription'

  // pollMs: docs/portals.md, 2-3 seconds while the operation is unfinished.
  let { pollMs = 2500 }: { pollMs?: number } = $props()

  let catalog = $state<Catalog | null>(null)
  let answer = $state<SubscriptionAnswer | null>(null)
  let loadError = $state('')

  let name = $state('')
  let flavor = $state('')
  let expiry = $state('')
  let ephemeral = $state(false)
  let formError = $state('')
  let busy = $state(false)

  let created = $state<{ name: string; shown: boolean } | null>(null)
  let operation = $state<Operation | null>(null)
  let pollFailed = $state(false)
  // Shown this once. It lives in this variable only, never in storage or a
  // URL, and is gone when the page is left or reloaded (docs/portals.md).
  let password = $state('')
  let copyNote = $state('')

  // A create with no answer may still have worked, so the same request
  // retried keeps its key and can't make a second database. Any answer ends
  // that: a refused create left nothing behind.
  let unanswered: { key: string; body: string } | null = null

  let timer: ReturnType<typeof setTimeout> | undefined
  let left = false

  onMount(async () => {
    try {
      const [c, sub] = await Promise.all([
        authed<Catalog>('/catalog'),
        authed<SubscriptionAnswer>('/subscription'),
      ])
      catalog = c
      answer = sub
      flavor = c.service_types[0]?.flavors[0]?.code ?? ''
    } catch (err) {
      loadError = (err as Error).message
    }
  })

  onDestroy(() => {
    left = true
    clearTimeout(timer)
  })

  const unfinished = (op: Operation) => op.status === 'pending' || op.status === 'running'

  async function follow(id: string) {
    try {
      operation = await authed<Operation>(`/operations/${id}`)
      pollFailed = false
    } catch {
      // Its progress is unknown, not bad: keep asking.
      pollFailed = true
    }
    if (!left && operation && unfinished(operation)) {
      timer = setTimeout(() => follow(id), pollMs)
    }
  }

  async function create(e: SubmitEvent) {
    e.preventDefault()
    if (busy) return
    busy = true
    formError = ''
    const body = JSON.stringify({
      name,
      flavor,
      ...(expiry ? { expires_at: new Date(expiry).toISOString(), ephemeral } : {}),
    })
    if (unanswered?.body !== body) unanswered = { key: crypto.randomUUID(), body }
    try {
      const res = await authed<{
        instance: Instance
        operation: Operation
        password?: string
      }>('/instances', {
        method: 'POST',
        body,
        headers: { 'Idempotency-Key': unanswered.key },
      })
      unanswered = null
      password = res.password ?? ''
      created = { name: res.instance.name, shown: res.password !== undefined }
      operation = res.operation
      timer = setTimeout(() => follow(res.operation.id), pollMs)
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 0)) unanswered = null
      formError = (err as Error).message
    } finally {
      busy = false
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(password)
      copyNote = 'Copied.'
    } catch {
      copyNote = 'Could not copy it. Select the password and copy it by hand.'
    }
  }
</script>

<main class="page">
  {#if created && operation}
    <h1>{created.name}</h1>

    <section class="card">
      <h2>Password</h2>
      {#if created.shown}
        <p class="warning" role="alert">
          This is the only time this password is shown. Save it now: leaving or reloading this
          page discards it.
        </p>
        <code>{password}</code>
        <p class="actions">
          <button type="button" onclick={copy}>Copy password</button>
          <span class="muted" role="status">{copyNote}</span>
        </p>
      {:else}
        <p role="alert">
          This database was already created by an earlier attempt, so its password can't be shown
          again. Reset the password once the database is running.
        </p>
      {/if}
    </section>

    <section class="card">
      <h2>Progress</h2>
      {#if operation.status === 'succeeded'}
        <p>The database is ready.</p>
      {:else if operation.status === 'failed'}
        <p class="failed" role="alert">{operation.message ?? 'The database could not be created.'}</p>
      {:else if operation.status === 'canceled'}
        <p class="failed" role="alert">{operation.message ?? 'The create was canceled.'}</p>
      {:else}
        <p>{words(operation.status)}…</p>
      {/if}
      {#if pollFailed}
        <p class="muted">Could not check its progress just now. Trying again.</p>
      {/if}
    </section>

    <p><a href="/" onclick={(e) => link(e, '/')}>Back to the dashboard</a></p>
  {:else}
    <h1>Create a database</h1>
    {#if loadError}
      <p class="failed" role="alert">{loadError}</p>
    {:else if !catalog || !answer}
      <p class="muted">Loading…</p>
    {:else if !answer.entitled || !answer.subscription}
      <p class="failed" role="alert">This account has no subscription in effect, so new databases can't be created.</p>
    {:else}
      <form class="card" onsubmit={create}>
        <label>
          Name
          <input type="text" required autocomplete="off" spellcheck="false" bind:value={name} />
          <small>Lowercase letters, digits and hyphens, starting with a letter.</small>
        </label>

        {#each catalog.service_types as type (type.code)}
          <fieldset>
            <legend>{type.name} size</legend>
            {#each type.flavors as f (f.code)}
              <label class="choice">
                <input type="radio" name="flavor" value={f.code} bind:group={flavor} />
                <span>
                  {f.name}: {f.storage_gb} GB{#if f.max_connections}, {f.max_connections} connections{/if}
                </span>
              </label>
            {/each}
          </fieldset>
        {/each}
        <p class="muted">Left on your plan: {leftWords(answer.subscription.limits)}.</p>

        <label>
          Expiry (optional)
          <input type="datetime-local" bind:value={expiry} />
          <small>At this date the database is stopped, and deleted 7 days later unless you set a later date.</small>
        </label>
        {#if expiry}
          <label class="choice">
            <input type="checkbox" bind:checked={ephemeral} />
            <span>Ephemeral: delete it at the expiry date, at once, with no final copy of its data.</span>
          </label>
        {/if}

        {#if formError}
          <p class="failed" role="alert">{formError}</p>
        {/if}
        <button type="submit" disabled={busy || !flavor}>Create database</button>
      </form>
      <p><a href="/" onclick={(e) => link(e, '/')}>Cancel</a></p>
    {/if}
  {/if}
</main>

<style>
  form {
    display: grid;
    gap: var(--space-4);
  }

  fieldset {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    border: 0;
  }

  legend {
    padding: 0 0 var(--space-1);
    color: var(--text-strong);
  }

  .choice {
    display: flex;
    gap: var(--space-2);
    align-items: baseline;
    color: var(--text);
  }

  code {
    display: block;
    padding: var(--space-2);
    overflow-wrap: anywhere;
    color: var(--text-strong);
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  .warning {
    color: var(--danger);
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }

  form p {
    margin: 0;
  }
</style>
