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

export type Connection = { host: string; port: number; database: string; user: string; tls: string }

export type Instance = {
  id: string
  name: string
  status: string
  health: string
  desired_state: string
  message?: string
  allocation: { storage_gb: number; max_connections?: number }
  connection: Connection | null
  operation: Operation | null
  storage: { used_bytes: number | null; allocated_bytes: number; measured_at: string | null; state: string }
  expires_at: string | null
  ephemeral: boolean
  created_at: string
}

export type Flavor = { code: string; name: string; storage_gb: number; max_connections?: number }
export type Catalog = { service_types: { code: string; name: string; flavors: Flavor[] }[] }

export type Operation = {
  id: string
  instance_id: string
  kind: string
  status: string
  message?: string
  requested_by: string
  created_at: string
}

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

// connectionString has no password: the platform doesn't have it to show.
export function connectionString(c: Connection): string {
  return `postgresql://${c.user}@${c.host}:${c.port}/${c.database}?sslmode=require`
}

export function bytesWords(bytes: number): string {
  const gib = bytes / 1024 ** 3
  if (gib >= 1) return `${Number(gib.toFixed(1))} GB`
  return `${Number((bytes / 1024 ** 2).toFixed(1))} MB`
}

// toDateTimeInput gives a time in the local form a datetime-local input takes.
export function toDateTimeInput(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const kinds: Record<string, string> = {
  create: 'Create',
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  delete: 'Delete',
  reset_password: 'Password reset',
  expire: 'Expiry',
}

export function kindWords(kind: string): string {
  return kinds[kind] ?? words(kind)
}

// requesterWords: the API says you, operator or system, never a login.
export function requesterWords(by: string): string {
  return { you: 'you', operator: 'the operator', system: 'the platform' }[by] ?? by
}

// words turns a status like past_due into text.
export function words(status: string): string {
  if (status === 'ok') return 'OK'
  const text = status.replaceAll('_', ' ')
  return text.charAt(0).toUpperCase() + text.slice(1)
}
