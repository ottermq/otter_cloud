<script lang="ts">
  import type { Snippet } from 'svelte'

  // A signed-out page: a title and a form, or, without onsubmit, a title and
  // whatever it has to say. What onsubmit throws is shown as written; the API
  // writes its errors for a person.
  let {
    title,
    action,
    onsubmit,
    children,
    footer,
  }: {
    title: string
    action?: string
    onsubmit?: () => Promise<void>
    children: Snippet
    footer?: Snippet
  } = $props()

  let error = $state('')
  let busy = $state(false)

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (busy) return
    busy = true
    error = ''
    try {
      await onsubmit?.()
    } catch (err) {
      error = (err as Error).message
    } finally {
      busy = false
    }
  }
</script>

<main>
  <h1>{title}</h1>
  {#if onsubmit}
    <form onsubmit={submit}>
      {@render children()}
      {#if error}
        <p class="error" role="alert">{error}</p>
      {/if}
      <button type="submit" disabled={busy}>{action}</button>
    </form>
  {:else}
    {@render children()}
  {/if}
  {#if footer}
    <footer>{@render footer()}</footer>
  {/if}
</main>

<style>
  main {
    max-width: 24rem;
    margin: var(--space-6) auto;
    padding: var(--space-6) var(--space-4);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  h1 {
    margin: 0 0 var(--space-4);
    font-size: 1.5rem;
    color: var(--text-strong);
  }

  form {
    display: grid;
    gap: var(--space-4);
  }

  .error {
    margin: 0;
    color: var(--danger);
  }

  footer {
    margin-top: var(--space-4);
    color: var(--text-muted);
    font-size: 0.875rem;
  }
</style>
