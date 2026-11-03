import { render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import Activity from './Activity.svelte'
import OperationList from './OperationList.svelte'

const failed = {
  id: 'op1',
  instance_id: 'i1',
  kind: 'reset_password',
  status: 'failed',
  reason: 'reset_password_failed',
  message: 'The password could not be reset. Your current password still works; try again in a few minutes.',
  requested_by: 'you',
  created_at: '2026-11-01T00:00:00Z',
  finished_at: '2026-11-01T00:01:00Z',
}
const gone = { ...failed, id: 'op2', instance_id: 'i2', kind: 'delete', status: 'succeeded', reason: undefined, message: undefined, requested_by: 'system' }

function stubAPI(answers: Record<string, [number, unknown]>) {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    const [status, body] = answers[url]
    return new Response(JSON.stringify(body), { status })
  }))
}

afterEach(() => vi.unstubAllGlobals())

describe('the operation list', () => {
  it('shows the kind in words, who asked, and the message the platform wrote', async () => {
    stubAPI({ '/api/instances/i1/operations': [200, { operations: [failed] }] })
    render(OperationList, { path: '/instances/i1/operations' })
    await screen.findByText('Password reset')
    expect(screen.getByText('Failed')).toBeTruthy()
    expect(screen.getByText(/by you$/)).toBeTruthy()
    expect(screen.getByText(/Your current password still works/)).toBeTruthy()
    expect(document.body.textContent).not.toContain('reset_password')
  })

  it('shows only what the API sends, and says when there is nothing', async () => {
    stubAPI({ '/api/operations': [200, { operations: [] }] })
    render(OperationList, { path: '/operations' })
    await screen.findByText('Nothing yet.')
  })

  it('shows an API error as written', async () => {
    stubAPI({ '/api/operations': [500, { error: 'internal error' }] })
    render(OperationList, { path: '/operations' })
    expect((await screen.findByRole('alert')).textContent).toBe('internal error')
  })
})

describe('the activity page', () => {
  it('names each database, and says when one is gone', async () => {
    stubAPI({
      '/api/operations': [200, { operations: [failed, gone] }],
      '/api/instances': [200, { instances: [{ id: 'i1', name: 'production' }] }],
    })
    render(Activity)
    await screen.findByText('production')
    expect(screen.getByText('A deleted database')).toBeTruthy()
    expect(screen.getByText(/by the platform$/)).toBeTruthy()
  })

  it('still lists the operations if the names can not be loaded', async () => {
    stubAPI({
      '/api/operations': [200, { operations: [failed] }],
      '/api/instances': [500, { error: 'internal error' }],
    })
    render(Activity)
    await screen.findByText('Password reset')
    expect(screen.queryByRole('alert')).toBeNull()
  })
})
