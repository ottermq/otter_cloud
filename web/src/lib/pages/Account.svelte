<script lang="ts">
  import { onMount } from 'svelte'
  import { authed, session } from '../session.svelte'

  type Session = {
    id: string
    created_at: string
    last_seen_at: string
    ip: string | null
    user_agent: string
    current: boolean
  }

  let current = $state('')
  let next = $state('')
  let repeat = $state('')
  let passwordError = $state('')
  let passwordDone = $state('')
  let changing = $state(false)

  let sessions = $state<Session[] | null>(null)
  let sessionsError = $state('')
  let sessionsNote = $state('')

  onMount(loadSessions)

  async function loadSessions() {
    try {
      const { sessions: list } = await authed<{ sessions: Session[] }>('/account/sessions')
      // This one first, then the rest newest first, as the API lists them.
      sessions = list.toSorted((a, b) => Number(b.current) - Number(a.current))
      sessionsError = ''
    } catch (err) {
      sessionsError = (err as Error).message
    }
  }

  async function changePassword(e: SubmitEvent) {
    e.preventDefault()
    if (changing) return
    passwordError = ''
    passwordDone = ''
    if (next !== repeat) {
      passwordError = 'The two new passwords are different.'
      return
    }
    changing = true
    try {
      await authed('/account/password', {
        method: 'POST',
        body: JSON.stringify({ current_password: current, new_password: next }),
      })
      current = next = repeat = ''
      passwordDone = 'Password changed. Your other sessions have ended.'
      await loadSessions()
    } catch (err) {
      passwordError = (err as Error).message
    } finally {
      changing = false
    }
  }

  async function endSession(id: string) {
    sessionsNote = ''
    sessionsError = ''
    try {
      await authed(`/account/sessions/${id}`, { method: 'DELETE' })
      await loadSessions()
    } catch (err) {
      sessionsError = (err as Error).message
    }
  }

  async function endOthers() {
    sessionsNote = ''
    sessionsError = ''
    try {
      const { ended } = await authed<{ ended: number }>('/account/sessions', { method: 'DELETE' })
      sessionsNote = `Ended ${ended} session${ended === 1 ? '' : 's'}.`
      await loadSessions()
    } catch (err) {
      sessionsError = (err as Error).message
    }
  }

  const date = (iso: string) => new Date(iso).toLocaleString()
  const others = $derived((sessions ?? []).filter((s) => !s.current).length)
</script>

<main class="page">
  <h1>Account</h1>

  <section class="card">
    <h2>Profile</h2>
    <p>{session.me?.email}</p>
  </section>

  <section class="card">
    <h2>Password</h2>
    <form onsubmit={changePassword}>
      <label>
        Current password
        <input type="password" autocomplete="current-password" required bind:value={current} />
      </label>
      <label>
        New password
        <input type="password" autocomplete="new-password" required bind:value={next} />
        <small>At least 12 characters.</small>
      </label>
      <label>
        Repeat the new password
        <input type="password" autocomplete="new-password" required bind:value={repeat} />
      </label>
      {#if passwordError}
        <p class="failed" role="alert">{passwordError}</p>
      {/if}
      {#if passwordDone}
        <p role="status">{passwordDone}</p>
      {/if}
      <p class="muted">Changing it ends your other sessions.</p>
      <button type="submit" disabled={changing}>Change password</button>
    </form>
  </section>

  <section class="card">
    <h2>Sessions</h2>
    {#if sessionsError}
      <p class="failed" role="alert">{sessionsError}</p>
    {/if}
    {#if sessionsNote}
      <p role="status">{sessionsNote}</p>
    {/if}
    {#if !sessions}
      {#if !sessionsError}<p class="muted">Loading…</p>{/if}
    {:else}
      <ul>
        {#each sessions as s (s.id)}
          <li>
            <span>{s.user_agent || 'Unknown browser'}</span>
            <span class="muted">
              {s.ip ?? 'Unknown address'} · last seen {date(s.last_seen_at)} · started {date(s.created_at)}
            </span>
            {#if s.current}
              <strong>This session</strong>
            {:else}
              <p><button type="button" class="plain" onclick={() => endSession(s.id)}>End this session</button></p>
            {/if}
          </li>
        {/each}
      </ul>
      {#if others > 0}
        <p class="end-all"><button type="button" onclick={endOthers}>End all other sessions</button></p>
      {/if}
    {/if}
  </section>
</main>

<style>
  form {
    display: grid;
    gap: var(--space-4);
  }

  form p {
    margin: 0;
  }

  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: grid;
    gap: var(--space-1);
    padding-bottom: var(--space-2);
    border-bottom: 1px solid var(--border);
    overflow-wrap: anywhere;
  }

  li p {
    margin: 0;
  }

  strong {
    color: var(--text-strong);
  }

  .plain {
    color: var(--text);
    background: none;
    border: 1px solid var(--border);
  }

  .end-all {
    margin: var(--space-4) 0 0;
  }
</style>
