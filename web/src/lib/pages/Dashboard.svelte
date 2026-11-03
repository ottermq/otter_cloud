<script lang="ts">
  import { onMount } from 'svelte'
  import { authed } from '../session.svelte'
  import {
    limitRows,
    priceWords,
    words,
    type Instance,
    type SubscriptionAnswer,
  } from '../subscription'

  let answer = $state<SubscriptionAnswer | null>(null)
  let databases = $state<Instance[]>([])
  let error = $state('')

  onMount(async () => {
    try {
      const [sub, list] = await Promise.all([
        authed<SubscriptionAnswer>('/subscription'),
        authed<{ instances: Instance[] }>('/instances'),
      ])
      answer = sub
      databases = list.instances
    } catch (err) {
      error = (err as Error).message
    }
  })

  const date = (iso: string) => new Date(iso).toLocaleDateString()
</script>

<main>
  <h1>Dashboard</h1>
  {#if error}
    <p class="failed" role="alert">{error}</p>
  {:else if !answer}
    <p class="muted">Loading…</p>
  {:else}
    <section>
      <h2>Subscription</h2>
      {#if answer.subscription}
        {@const sub = answer.subscription}
        <p>
          <strong>{sub.plan.name}</strong> · {priceWords(sub.plan)}
          {#if sub.ends_at}· until {date(sub.ends_at)}{/if}
        </p>
        {#if !answer.entitled}
          <p class="failed" role="alert">
            This subscription is not in effect ({words(sub.status)}, payment
            {words(sub.payment_status).toLowerCase()}), so new databases can't be created.
          </p>
        {/if}
        <dl>
          {#each limitRows(sub.limits) as row (row.key)}
            <div>
              <dt>{row.label}</dt>
              <dd>
                {row.usage}
                <progress value={row.allocated} max={row.allowed} aria-label={row.label}></progress>
              </dd>
            </div>
          {/each}
        </dl>
      {:else}
        <p class="failed" role="alert">This account has no subscription, so new databases can't be created.</p>
      {/if}
    </section>

    <section>
      <h2>Databases</h2>
      {#if databases.length === 0}
        <p class="muted">No databases yet.</p>
      {:else}
        <ul>
          {#each databases as db (db.id)}
            <li>
              <strong>{db.name}</strong>
              <span>{words(db.status)} · health: {words(db.health).toLowerCase()}</span>
              {#if db.operation}
                <span class="muted">{words(db.operation.kind)} in progress</span>
              {/if}
              {#if db.message}
                <span class="muted">{db.message}</span>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  {/if}
</main>

<style>
  main {
    max-width: 48rem;
    margin: 0 auto;
    padding: var(--space-6) var(--space-4);
  }

  h1 {
    margin: 0 0 var(--space-4);
    font-size: 1.5rem;
    color: var(--text-strong);
  }

  h2 {
    margin: 0 0 var(--space-2);
    font-size: 1.125rem;
    color: var(--text-strong);
  }

  section {
    margin-bottom: var(--space-4);
    padding: var(--space-4);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  p {
    margin: 0 0 var(--space-2);
  }

  strong {
    color: var(--text-strong);
    overflow-wrap: anywhere;
  }

  dl {
    display: grid;
    gap: var(--space-2);
    margin: var(--space-4) 0 0;
  }

  dt {
    color: var(--text-muted);
  }

  dd {
    display: grid;
    gap: var(--space-1);
    margin: 0;
  }

  progress {
    width: 100%;
    height: var(--space-2);
    accent-color: var(--accent);
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
    padding-bottom: var(--space-2);
    border-bottom: 1px solid var(--border);
  }

  li:last-child {
    padding-bottom: 0;
    border-bottom: 0;
  }

  .muted {
    color: var(--text-muted);
  }

  .failed {
    color: var(--danger);
  }
</style>
