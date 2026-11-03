// The path alone: the server answers only the portal's own URLs
// (internal/httpapi/portal.go), and pages take no query strings.
export const route = $state({ path: window.location.pathname })

export function navigate(to: string, replace = false): void {
  if (to === route.path) return
  if (replace) history.replaceState({}, '', to)
  else history.pushState({}, '', to)
  route.path = to
}

// Lives as long as the page does.
window.addEventListener('popstate', () => {
  route.path = window.location.pathname
})

// link follows an in-app link without a page load, leaving modified clicks
// (open in a new tab, say) to the browser.
export function link(e: MouseEvent, to: string): void {
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  e.preventDefault()
  navigate(to)
}

// takeFragment returns a value from the URL's fragment, where an invite or
// reset link carries its token, and removes the fragment: the token shouldn't
// stay in the address bar, to be copied along with it. A reload then finds no
// token, and the page asks for the link again.
export function takeFragment(name: string): string {
  const value = new URLSearchParams(window.location.hash.slice(1)).get(name) ?? ''
  if (window.location.hash !== '') history.replaceState({}, '', window.location.pathname)
  return value
}
