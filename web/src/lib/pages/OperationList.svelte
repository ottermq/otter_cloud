<script lang="ts">
  import { onMount } from 'svelte'
  import { authed } from '../session.svelte'
  import { kindWords, requesterWords, words, type Operation } from '../subscription'

  // What the API shows of operations: never a step, a node or an internal
  // error, only the message the platform wrote for the customer. nameOf, on
  // the account's list, says which database each one was on.
  let { path, nameOf }: { path: string; nameOf?: (op: Operation) => Promise<string> | string } = $props()

  let operations = $state<(Operation & { database?: string })[] | null>(null)
  let error = $state('')

  onMount(async () => {
    try {
      const res = await authed<{ operations: Operation[] }>(path)
      operations = await Promise.all(
        res.operations.map(async (op) => ({ ...op, database: await nameOf?.(op) })),
      )
    } catch (err) {
      error = (err as Error).message
    }
  })

  const date = (iso: string) => new Date(iso).toLocaleString()
</script>

{#if error}
  <p class="failed" role="alert">{error}</p>
{:else if !operations}
  <p class="muted">Loading…</p>
{:else if operations.length === 0}
  <p class="muted">Nothing yet.</p>
{:else}
  <ul>
    {#each operations as op (op.id)}
      <li>
        <strong>{kindWords(op.kind)}</strong>
        {#if op.database}<span>{op.database}</span>{/if}
        <span class:failed={op.status === 'failed'}>{words(op.status)}</span>
        <span class="muted">{date(op.created_at)} · by {requesterWords(op.requested_by)}</span>
        {#if op.message}
          <span class:failed={op.status === 'failed'}>{op.message}</span>
        {/if}
      </li>
    {/each}
  </ul>
{/if}

<style>
  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: grid;
    padding-bottom: var(--space-2);
    border-bottom: 1px solid var(--border);
    overflow-wrap: anywhere;
  }

  li:last-child {
    padding-bottom: 0;
    border-bottom: 0;
  }

  strong {
    color: var(--text-strong);
  }
</style>
