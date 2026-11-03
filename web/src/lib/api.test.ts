import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './api'

function answer(status: number, body: string) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(status === 204 ? null : body, { status })))
}

afterEach(() => vi.unstubAllGlobals())

describe('api', () => {
  it('returns the JSON', async () => {
    answer(200, '{"email":"ana@example.com"}')
    await expect(api('/me')).resolves.toEqual({ email: 'ana@example.com' })
    expect(fetch).toHaveBeenCalledWith('/api/me', expect.objectContaining({
      headers: { 'Content-Type': 'application/json' },
    }))
  })

  it('shows the API error as written', async () => {
    answer(409, '{"error":"another operation is in flight"}')
    const err = await api('/instances').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(409)
    expect((err as ApiError).message).toBe('another operation is in flight')
  })

  it('has a message for a failure that is not the API', async () => {
    answer(502, '<html>Bad Gateway</html>')
    await expect(api('/me')).rejects.toThrow('The request failed (502).')
  })

  it('has a message for no answer at all', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    const err = await api('/me').catch((e: unknown) => e)
    expect((err as ApiError).status).toBe(0)
    expect((err as ApiError).message).toMatch(/Could not reach/)
  })

  it('takes a 204 with no body', async () => {
    answer(204, '')
    await expect(api('/logout', { method: 'POST' })).resolves.toBeUndefined()
  })
})
