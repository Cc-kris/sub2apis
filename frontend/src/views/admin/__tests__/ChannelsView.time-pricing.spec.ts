import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../ChannelsView.vue'),
  'utf8'
)

describe('ChannelsView time pricing contract', () => {
  it('persists interval rules and service-tier multipliers', () => {
    expect(source).toContain('time_pricing: entry.time_pricing || null')
    expect(source).toContain('fast_multiplier: entry.fast_multiplier != null')
    expect(source).toContain('flex_multiplier: entry.flex_multiplier != null')
    expect(source).toContain('findModelConflict(allModels)')
  })
})
