<script lang="ts">
  import { onMount } from 'svelte'
  import { api, ApiError } from './lib/api'

  type Me = { email: string }

  // The sign-in pages come with #109: for now, what the session says.
  let status = $state('Checking your session…')
  let failed = $state(false)

  onMount(async () => {
    try {
      const me = await api<Me>('/me')
      status = `Signed in as ${me.email}.`
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        status = 'You are not signed in.'
      } else {
        failed = true
        status = err instanceof Error ? err.message : 'Something went wrong.'
      }
    }
  })
</script>

<main>
  <h1>Otter Labs Cloud</h1>
  <p class:failed>{status}</p>
</main>

<style>
  main {
    max-width: 32rem;
    margin: var(--space-6) auto;
    padding: var(--space-6) var(--space-4);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  h1 {
    margin: 0 0 var(--space-2);
    font-size: 1.5rem;
    color: var(--text-strong);
  }

  p {
    margin: 0;
    color: var(--text-muted);
  }

  .failed {
    color: var(--danger);
  }
</style>
