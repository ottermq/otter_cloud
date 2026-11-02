<script lang="ts">
  import { api } from '../api'
  import { takeFragment } from '../router.svelte'
  import { signedIn, type Me } from '../session.svelte'
  import SignedOutPage from './SignedOutPage.svelte'

  const token = takeFragment('token')

  let password = $state('')
  let repeat = $state('')

  async function reset() {
    if (password !== repeat) throw new Error('The two passwords are different.')
    signedIn(await api<Me>('/password-reset', { method: 'POST', body: JSON.stringify({ token, password }) }))
  }
</script>

{#if token}
  <SignedOutPage title="Set a new password" action="Set password" onsubmit={reset}>
    <label>
      New password
      <input type="password" autocomplete="new-password" required bind:value={password} />
      <small>At least 12 characters.</small>
    </label>
    <label>
      Repeat the password
      <input type="password" autocomplete="new-password" required bind:value={repeat} />
    </label>
  </SignedOutPage>
{:else}
  <SignedOutPage title="Set a new password">
    <p role="alert">This page needs the whole reset link. Open the link you were given again.</p>
  </SignedOutPage>
{/if}
