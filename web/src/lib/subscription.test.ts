import { describe, expect, it } from 'vitest'
import { leftWords, limitRows, priceWords, words } from './subscription'

describe('limitRows', () => {
  it('keeps a fixed order and the units', () => {
    const rows = limitRows({
      connections: { allowed: 40, allocated: 20 },
      storage_gb: { allowed: 10, allocated: 2 },
      instances: { allowed: 2, allocated: 1 },
    })
    expect(rows.map((r) => [r.label, r.usage])).toEqual([
      ['Databases', '1 of 2'],
      ['Storage', '2 of 10 GB'],
      ['Connections', '20 of 40'],
    ])
  })

  it('still shows a limit it has no words for', () => {
    expect(limitRows({ gpus: { allowed: 1, allocated: 0 } }).map((r) => r.label)).toEqual(['gpus'])
  })
})

it('leftWords', () => {
  expect(leftWords({
    instances: { allowed: 2, allocated: 1 },
    storage_gb: { allowed: 10, allocated: 2 },
    connections: { allowed: 60, allocated: 20 },
  })).toBe('1 database, 8 GB, 40 connections')
})

describe('priceWords', () => {
  const plan = { name: 'Plan', currency: 'BRL', interval: 'month' }

  it('calls a price of zero free', () => {
    expect(priceWords({ ...plan, price_minor: 0, interval: 'none' })).toBe('Free')
  })

  it('shows the interval', () => {
    expect(priceWords({ ...plan, price_minor: 4990 })).toMatch(/49[.,]90 a month$/)
  })
})

it('words', () => {
  expect(words('past_due')).toBe('Past due')
})
