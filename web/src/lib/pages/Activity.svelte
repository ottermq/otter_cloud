<script lang="ts">
  import { authed } from '../session.svelte'
  import type { Instance, Operation } from '../subscription'
  import OperationList from './OperationList.svelte'

  // The API's list leaves deleted databases out, and their operations stay.
  const names = authed<{ instances: Instance[] }>('/instances').then(
    (res) => new Map(res.instances.map((db) => [db.id, db.name])),
    () => new Map<string, string>(),
  )

  async function nameOf(op: Operation) {
    return (await names).get(op.instance_id) ?? 'A deleted database'
  }
</script>

<main class="page">
  <h1>Activity</h1>
  <section class="card">
    <OperationList path="/operations" {nameOf} />
  </section>
</main>
