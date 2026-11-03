<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import { ApiError } from '../api'
  import { RetryKey } from '../idempotency'
  import { link } from '../router.svelte'
  import { authed } from '../session.svelte'
  import {
    bytesWords,
    connectionString,
    toDateTimeInput,
    words,
    type Instance,
    type Operation,
  } from '../subscription'
  import CopyButton from './CopyButton.svelte'
  import OneTimePassword from './OneTimePassword.svelte'
  import OperationList from './OperationList.svelte'

  // pollMs: docs/portals.md, 2-3 seconds while an operation runs.
  let { id, pollMs = 2500 }: { id: string; pollMs?: number } = $props()

  type Accepted = { instance: Instance; operation: Operation; password?: string }

  let db = $state<Instance | null>(null)
  let loadError = $state('')
  let actionError = $state('')
  let busy = $state(false)
  let confirming = $state<'stop' | 'reset' | 'delete' | null>(null)
  let typedName = $state('')
  let expiry = $state('')

  // Shown this once (OneTimePassword).
  let reset = $state<{ password: string } | null>(null)
  const resetKey = new RetryKey()
  let noAnswer = false

  // Bumped to reload the activity when an operation starts or ends.
  let activity = $state(0)

  let followed = ''
  let timer: ReturnType<typeof setTimeout> | undefined
  let left = false

  onMount(() => refresh())

  onDestroy(() => {
    left = true
    clearTimeout(timer)
  })

  function show(next: Instance) {
    const first = db === null
    db = next
    if (first) expiry = next.expires_at ? toDateTimeInput(next.expires_at) : ''
  }

  async function refresh() {
    clearTimeout(timer)
    try {
      show(await authed<Instance>(`/instances/${id}`))
      loadError = ''
    } catch (err) {
      // With the page already up, a failed check says nothing about the
      // database: keep what is shown and ask again.
      if (!db) {
        loadError = (err as Error).message
        return
      }
    }
    await settle()
    if (!left && db?.operation) timer = setTimeout(refresh, pollMs)
  }

  // An operation the page was following has finished. The instance no longer
  // names it, so ask the operation how it went.
  async function settle() {
    if (db?.operation) {
      followed = db.operation.id
      return
    }
    if (!followed) return
    const opId = followed
    followed = ''
    activity++
    try {
      const op = await authed<Operation>(`/operations/${opId}`)
      if (op.status !== 'succeeded') {
        if (op.message) actionError = op.message
        // The new password never took effect: don't leave it to be saved.
        if (op.kind === 'reset_password') reset = null
      }
    } catch {
      // The status above still shows where the database is.
    }
  }

  async function act(path: string, init: RequestInit = {}): Promise<Accepted | null> {
    busy = true
    actionError = ''
    noAnswer = false
    try {
      const res = await authed<Accepted>(path, { method: 'POST', ...init })
      confirming = null
      show(res.instance)
      followed = res.operation.id
      activity++
      clearTimeout(timer)
      timer = setTimeout(refresh, pollMs)
      return res
    } catch (err) {
      actionError = (err as Error).message
      noAnswer = err instanceof ApiError && err.status === 0
      return null
    } finally {
      busy = false
    }
  }

  async function resetPassword() {
    const res = await act(`/instances/${id}/reset-password`, {
      headers: { 'Idempotency-Key': resetKey.for('') },
    })
    if (!noAnswer) resetKey.answered()
    if (res) reset = { password: res.password ?? '' }
  }

  async function saveExpiry(value: string | null) {
    busy = true
    actionError = ''
    try {
      const next = await authed<Instance>(`/instances/${id}`, {
        method: 'PATCH',
        body: JSON.stringify({ expires_at: value }),
      })
      db = next
      expiry = next.expires_at ? toDateTimeInput(next.expires_at) : ''
    } catch (err) {
      actionError = (err as Error).message
    } finally {
      busy = false
    }
  }

  const date = (iso: string) => new Date(iso).toLocaleString()
  const inFlight = $derived(db?.operation != null)
  const gone = $derived(db?.status === 'deleting' || db?.status === 'deleted' || db?.desired_state === 'deleted')
</script>

<main class="page">
  {#if loadError}
    <h1>Database</h1>
    <p class="failed" role="alert">{loadError}</p>
    <p><a href="/" onclick={(e) => link(e, '/')}>Back to the dashboard</a></p>
  {:else if !db}
    <p class="muted">Loading…</p>
  {:else}
    <h1>{db.name}</h1>

    {#if reset}
      <OneTimePassword
        password={reset.password}
        replayed="This password was already reset by an earlier attempt, so the new one can't be shown again. Reset it again."
      />
    {/if}

    <section class="card">
      <h2>Overview</h2>
      <dl>
        <div><dt>Status</dt><dd>{words(db.status)}</dd></div>
        <div><dt>Health</dt><dd>{words(db.health)}</dd></div>
        <div>
          <dt>Allocation</dt>
          <dd>
            {db.allocation.storage_gb} GB{#if db.allocation.max_connections}, {db.allocation.max_connections} connections{/if}
          </dd>
        </div>
        <div>
          <dt>Storage used</dt>
          <dd>
            {#if db.storage.used_bytes === null}
              Not measured yet
            {:else}
              {bytesWords(db.storage.used_bytes)} of {bytesWords(db.storage.allocated_bytes)}
            {/if}
          </dd>
        </div>
        <div><dt>Created</dt><dd>{date(db.created_at)}</dd></div>
        {#if db.ephemeral}
          <div><dt>Ephemeral</dt><dd>Deleted at its expiry, with no final copy</dd></div>
        {/if}
      </dl>
      {#if db.operation}
        <p class="muted" role="status">{words(db.operation.kind)} in progress…</p>
      {/if}
      {#if db.storage.state === 'warning'}
        <p class="failed" role="alert">
          This database is using over 80% of its storage. Past its allocation it becomes read-only,
          though not at the exact moment it passes: storage is measured from time to time.
        </p>
      {/if}
      {#if db.message}
        <p class="failed" role="alert">{db.message}</p>
      {/if}
      {#if actionError}
        <p class="failed" role="alert">{actionError}</p>
      {/if}
    </section>

    {#if db.status === 'deleted'}
      <p>This database was deleted.</p>
    {:else}
      <section class="card">
        <h2>Connection</h2>
        {#if db.connection}
          {@const c = db.connection}
          <dl>
            <div><dt>Host</dt><dd>{c.host}</dd></div>
            <div><dt>Port</dt><dd>{c.port}</dd></div>
            <div><dt>Database</dt><dd>{c.database}</dd></div>
            <div><dt>User</dt><dd>{c.user}</dd></div>
            <div><dt>TLS</dt><dd>{words(c.tls)}</dd></div>
          </dl>
          <code>{connectionString(c)}</code>
          <CopyButton text={connectionString(c)} label="Copy connection string" />
          <p class="muted">
            Add your password to connect. To verify the server's certificate as well, use
            <code class="inline">sslmode=verify-full</code>, with
            <code class="inline">sslrootcert=system</code> on libpq 16 or later.
          </p>
        {:else}
          <p class="muted">Connection details appear while the database is running.</p>
        {/if}
      </section>

      {#if !gone}
        <section class="card">
          <h2>Actions</h2>
          {#if confirming === 'stop'}
            <p>
              Stopping refuses new connections and closes the open ones. No server stops, and the
              data stays. It keeps counting against your plan.
            </p>
            <p class="row">
              <button type="button" disabled={busy || inFlight} onclick={() => act(`/instances/${id}/stop`)}>Stop database</button>
              <button type="button" class="plain" onclick={() => (confirming = null)}>Keep it running</button>
            </p>
          {:else if confirming === 'reset'}
            <p>This replaces the current password. Applications using it will need the new one.</p>
            <p class="row">
              <button type="button" disabled={busy || inFlight} onclick={resetPassword}>Reset password</button>
              <button type="button" class="plain" onclick={() => (confirming = null)}>Keep the password</button>
            </p>
          {:else if confirming === 'delete'}
            <p>
              {#if db.ephemeral}
                This deletes the database and its data, and no final copy is kept.
              {:else}
                This deletes the database and its data. A final copy is kept for 3 days, and only
                the operator can restore it, on request.
              {/if}
            </p>
            <label>
              Type {db.name} to confirm
              <input type="text" autocomplete="off" spellcheck="false" bind:value={typedName} />
            </label>
            <p class="row">
              <button
                type="button"
                class="danger"
                disabled={busy || inFlight || typedName !== db.name}
                onclick={() => act(`/instances/${id}`, { method: 'DELETE', body: JSON.stringify({ confirm: typedName }) })}
              >Delete database</button>
              <button type="button" class="plain" onclick={() => { confirming = null; typedName = '' }}>Keep the database</button>
            </p>
          {:else}
            <p class="row">
              {#if db.status === 'running'}
                <button type="button" disabled={busy || inFlight} onclick={() => (confirming = 'stop')}>Stop</button>
              {:else if db.status === 'stopped'}
                <button type="button" disabled={busy || inFlight} onclick={() => act(`/instances/${id}/start`)}>Start</button>
              {/if}
              {#if db.status === 'running' || db.status === 'stopped'}
                <button type="button" disabled={busy || inFlight} onclick={() => (confirming = 'reset')}>Reset password</button>
              {/if}
              <button type="button" class="danger" disabled={busy || inFlight} onclick={() => (confirming = 'delete')}>Delete</button>
            </p>
          {/if}
        </section>

        <section class="card">
          <h2>Expiry</h2>
          <label>
            {db.expires_at ? `Expires ${date(db.expires_at)}` : 'No expiry'}
            <input type="datetime-local" bind:value={expiry} />
            <small>At this date the database is stopped, and deleted 7 days later unless you set a later date.</small>
          </label>
          <p class="row">
            <button
              type="button"
              disabled={busy || !expiry}
              onclick={() => saveExpiry(new Date(expiry).toISOString())}
            >Set expiry</button>
            {#if db.expires_at}
              <button type="button" class="plain" disabled={busy} onclick={() => saveExpiry(null)}>Clear expiry</button>
            {/if}
          </p>
        </section>
      {/if}
    {/if}

    <section class="card">
      <h2>Activity</h2>
      {#key activity}
        <OperationList path={`/instances/${id}/operations`} />
      {/key}
    </section>

    <p><a href="/" onclick={(e) => link(e, '/')}>Back to the dashboard</a></p>
  {/if}
</main>

<style>
  dl {
    display: grid;
    gap: var(--space-2);
    margin: 0 0 var(--space-2);
  }

  dl div {
    display: grid;
    grid-template-columns: 8rem 1fr;
    gap: var(--space-2);
  }

  dt {
    color: var(--text-muted);
  }

  dd {
    margin: 0;
    overflow-wrap: anywhere;
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

  code.inline {
    display: inline;
    padding: 0;
    border: 0;
    background: none;
  }

  .row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin: var(--space-2) 0 0;
  }

  .plain {
    color: var(--text);
    background: none;
    border: 1px solid var(--border);
  }

  .danger {
    color: var(--on-danger);
    background: var(--danger);
  }
</style>
