<script lang="ts">
  import { api } from '../api'
  import { takeFragment } from '../router.svelte'
  import { signedIn, type Me } from '../session.svelte'
  import SignedOutPage from './SignedOutPage.svelte'

  const token = takeFragment('invite')

  let accountName = $state('')
  let password = $state('')
  let repeat = $state('')

  async function signUp() {
    // There is no email to recover a mistyped password with.
    if (password !== repeat) throw new Error('The two passwords are different.')
    signedIn(await api<Me>('/signup', {
      method: 'POST',
      body: JSON.stringify({ token, password, account_name: accountName }),
    }))
  }
</script>

{#if token}
  <SignedOutPage title="Create your account" action="Create account" onsubmit={signUp}>
    <label>
      Account name (optional)
      <input type="text" autocomplete="organization" bind:value={accountName} />
    </label>
    <label>
      Password
      <input type="password" autocomplete="new-password" required bind:value={password} />
      <small>At least 12 characters.</small>
    </label>
    <label>
      Repeat the password
      <input type="password" autocomplete="new-password" required bind:value={repeat} />
    </label>
  </SignedOutPage>
{:else}
  <SignedOutPage title="Create your account">
    <p role="alert">This page needs the whole invite link. Open the link you were given again.</p>
  </SignedOutPage>
{/if}
