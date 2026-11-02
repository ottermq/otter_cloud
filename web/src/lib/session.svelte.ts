import { api, ApiError } from './api'
import { navigate, route } from './router.svelte'

// Me is the logged-in user, as /api/me, login and sign-up return it.
export type Me = { email: string }

export const session = $state<{ checked: boolean; me: Me | null; error: string }>({
  checked: false,
  me: null,
  error: '',
})

// Where to go after logging in: the page that asked for it. Kept here, not in
// the URL, so no link can choose where a login lands.
let returnTo = '/'

// loadSession asks who is logged in. A 401 is the ordinary signed-out state;
// anything else says nothing about the session, and is shown instead of
// sending a logged-in user to the login page.
export async function loadSession(): Promise<void> {
  try {
    session.me = await api<Me>('/me')
  } catch (err) {
    if (!(err instanceof ApiError && err.status === 401)) session.error = (err as Error).message
  }
  session.checked = true
}

export function requireLogin(): void {
  returnTo = route.path
  navigate('/login', true)
}

export function signedIn(me: Me): void {
  session.me = me
  const to = returnTo
  returnTo = '/'
  navigate(to, true)
}

// signOut throws if the API refuses, and the user stays logged in.
export async function signOut(): Promise<void> {
  await api('/logout', { method: 'POST' })
  navigate('/login')
  session.me = null
}
