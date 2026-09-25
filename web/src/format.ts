export function timeAgo(iso: string | undefined, now = Date.now()): string {
  if (!iso) return ''
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000))
  if (s < 45) return 'just now'
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.round(h / 24)
  return `${d}d ago`
}

export function duration(from?: string, to?: string, now = Date.now()): string {
  if (!from) return ''
  const end = to ? new Date(to).getTime() : now
  let s = Math.max(0, Math.round((end - new Date(from).getTime()) / 1000))
  const h = Math.floor(s / 3600)
  s -= h * 3600
  const m = Math.floor(s / 60)
  s -= m * 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function dateTime(iso: string): string {
  return new Date(iso).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })
}

/** Shortens EKS ARNs to "name (region)" for display. */
export function shortContext(ctx: string): string {
  const m = /^arn:aws[a-z-]*:eks:([a-z0-9-]+):\d{12}:cluster\/(.+)$/.exec(ctx)
  return m ? `${m[2]} (${m[1]})` : ctx
}

export function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}
