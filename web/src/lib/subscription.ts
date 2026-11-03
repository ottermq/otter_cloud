// What GET /api/subscription and GET /api/instances return. The server decides
// entitlement and usage; these only put them into words.
export type Limit = { allowed: number; allocated: number }

export type Subscription = {
  plan: { name: string; price_minor: number; currency: string; interval: string }
  status: string
  payment_status: string
  starts_at: string
  ends_at: string | null
  limits: Record<string, Limit>
}

export type SubscriptionAnswer = { entitled: boolean; subscription: Subscription | null }

export type Instance = {
  id: string
  name: string
  status: string
  health: string
  message?: string
  operation: Operation | null
}

export type Flavor = { code: string; name: string; storage_gb: number; max_connections?: number }
export type Catalog = { service_types: { code: string; name: string; flavors: Flavor[] }[] }

export type Operation = { id: string; kind: string; status: string; message?: string }

const limitWords: [key: string, label: string, unit: string][] = [
  ['instances', 'Databases', ''],
  ['storage_gb', 'Storage', ' GB'],
  ['connections', 'Connections', ''],
  ['memory_mb', 'Memory', ' MB'],
  ['vcpu', 'vCPUs', ''],
]

// limitRows lists the plan's limits in a fixed order, each with its usage in
// words. A limit this portal has no words for still shows, under its key.
export function limitRows(limits: Record<string, Limit>) {
  const known = limitWords.map(([key]) => key)
  const others = Object.keys(limits).filter((key) => !known.includes(key)).sort()
  return [...limitWords, ...others.map((key): [string, string, string] => [key, key, ''])]
    .filter(([key]) => key in limits)
    .map(([key, label, unit]) => ({
      key,
      label,
      allowed: limits[key].allowed,
      allocated: limits[key].allocated,
      usage: `${limits[key].allocated} of ${limits[key].allowed}${unit}`,
    }))
}

// leftWords says what the plan still has room for. It is for reading: whether
// a create fits is the API's to decide.
export function leftWords(limits: Record<string, Limit>): string {
  const left = (key: string) => limits[key].allowed - limits[key].allocated
  const parts = []
  if (limits.instances) parts.push(`${left('instances')} database${left('instances') === 1 ? '' : 's'}`)
  if (limits.storage_gb) parts.push(`${left('storage_gb')} GB`)
  if (limits.connections) parts.push(`${left('connections')} connections`)
  return parts.join(', ')
}

export function priceWords(plan: Subscription['plan']): string {
  if (plan.price_minor === 0) return 'Free'
  const price = new Intl.NumberFormat(undefined, { style: 'currency', currency: plan.currency })
    .format(plan.price_minor / 100)
  return plan.interval === 'month' ? `${price} a month` : price
}

// words turns a status like past_due into text.
export function words(status: string): string {
  const text = status.replaceAll('_', ' ')
  return text.charAt(0).toUpperCase() + text.slice(1)
}
