// ApiError carries the API's own message: {"error": "…"} is written for a
// person and shown as it is (security.md, "Errors never leak").
export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

// api calls the API on this origin, with the session cookie. A failure
// without the API's JSON (a proxy's error page, say) gets a generic message.
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(`/api${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init.headers },
  })
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    throw new ApiError(res.status, body?.error ?? `The request failed (${res.status}).`)
  }
  if (res.status === 204) return undefined as T
  return res.json()
}
