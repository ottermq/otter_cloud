import { describe, expect, it } from 'vitest'
import { bytesWords, connectionString, kindWords, leftWords, limitRows, priceWords, requesterWords, toDateTimeInput, words } from './subscription'

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
  expect(words('ok')).toBe('OK')
})

it('connectionString has no password and asks for TLS', () => {
  expect(connectionString({ host: 'h.example.com', port: 5432, database: 'd', user: 'u', tls: 'required' }))
    .toBe('postgresql://u@h.example.com:5432/d?sslmode=require')
})

it('bytesWords', () => {
  expect(bytesWords(2147483648)).toBe('2 GB')
  expect(bytesWords(1610612736)).toBe('1.5 GB')
  expect(bytesWords(52428800)).toBe('50 MB')
})

it('toDateTimeInput is the local time, to the minute', () => {
  const d = new Date(2030, 0, 2, 3, 4)
  expect(toDateTimeInput(d.toISOString())).toBe('2030-01-02T03:04')
})

it('kindWords and requesterWords', () => {
  expect(kindWords('reset_password')).toBe('Password reset')
  expect(kindWords('something_new')).toBe('Something new')
  expect(requesterWords('system')).toBe('the platform')
})
