export type LatencyHealth = 'missing' | 'good' | 'warn' | 'slow' | 'critical' | 'error'
export type LatencyMetric = 'first_token' | 'duration'

export const FIRST_TOKEN_THRESHOLDS_MS = {
  warn: 10_000,
  slow: 30_000,
  critical: 60_000,
} as const

export const DURATION_THRESHOLDS_MS = {
  warn: 60_000,
  slow: 180_000,
  critical: 300_000,
} as const

function trimDecimal(value: number): string {
  return value.toFixed(2).replace(/\.00$/, '').replace(/(\.\d)0$/, '$1')
}

export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '-'
  if (ms < 60_000) return `${trimDecimal(ms / 1000)}s`
  const totalSeconds = Math.floor(ms / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  if (hours > 0) return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`
  return seconds > 0 ? `${minutes}m ${seconds}s` : `${minutes}m`
}

export function latencyHealth(durationMs: number | null | undefined, isError = false, metric: LatencyMetric = 'duration'): { state: LatencyHealth; width: number } {
  const thresholds = metric === 'first_token' ? FIRST_TOKEN_THRESHOLDS_MS : DURATION_THRESHOLDS_MS
  const width = durationMs == null ? 0 : Math.min(Math.max(durationMs / thresholds.critical * 100, 0), 100)
  if (isError) return { state: 'error', width }
  if (durationMs == null || !Number.isFinite(durationMs) || durationMs < 0) return { state: 'missing', width: 0 }
  const state: LatencyHealth = durationMs >= thresholds.critical
    ? 'critical'
    : durationMs >= thresholds.slow
      ? 'slow'
      : durationMs >= thresholds.warn
        ? 'warn'
        : 'good'
  return { state, width }
}
