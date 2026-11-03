<script lang="ts">
  import { onMount } from 'svelte'
  import CreateDatabase from './lib/pages/CreateDatabase.svelte'
  import Dashboard from './lib/pages/Dashboard.svelte'
  import Login from './lib/pages/Login.svelte'
  import ResetPassword from './lib/pages/ResetPassword.svelte'
  import SignUp from './lib/pages/SignUp.svelte'
  import { navigate, route } from './lib/router.svelte'
  import { loadSession, requireLogin, session, signOut } from './lib/session.svelte'

  // Invite and reset links work whoever is logged in: the session is replaced
  // by the one they start.
  const linkPages = ['/signup', '/reset-password']

  let signOutError = $state('')

  onMount(loadSession)

  $effect(() => {
    if (!session.checked || session.error || linkPages.includes(route.path)) return
    if (!session.me && route.path !== '/login') requireLogin()
    if (session.me && route.path === '/login') navigate('/', true)
  })

  async function logout() {
    signOutError = ''
    try {
      await signOut()
    } catch (err) {
      signOutError = (err as Error).message
    }
  }
</script>

{#if route.path === '/signup'}
  <SignUp />
{:else if route.path === '/reset-password'}
  <ResetPassword />
{:else if !session.checked}
  <p class="status">Checking your session…</p>
{:else if session.error}
  <p class="status failed" role="alert">{session.error}</p>
{:else if session.me}
  <header>
    <strong>Otter Labs Cloud</strong>
    <span>{session.me.email}</span>
    <button type="button" onclick={logout}>Log out</button>
  </header>
  {#if signOutError}
    <p class="status failed" role="alert">{signOutError}</p>
  {/if}
  {#if route.path === '/databases/new'}
    <CreateDatabase />
  {:else}
    <!-- Every other signed-in URL, until its own page exists (#112-#114). -->
    <Dashboard />
  {/if}
{:else if route.path === '/login'}
  <Login />
{/if}

<style>
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-4);
    background: var(--surface);
    border-bottom: 1px solid var(--border);
  }

  strong {
    margin-right: auto;
    color: var(--text-strong);
  }

  span {
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }

  .status {
    margin: var(--space-6) var(--space-4);
    text-align: center;
    color: var(--text-muted);
  }

  .failed {
    color: var(--danger);
  }
</style>
