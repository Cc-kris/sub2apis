import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../UsageView.vue'), 'utf8')

describe('UsageView export', () => {
  it('exports raw milliseconds alongside formatted billing values', () => {
    expect(source).toContain('log.first_token_ms ?? \'\'')
    expect(source).toContain('log.duration_ms')
    expect(source).toContain('exportToExcel')
    expect(source).toContain('exportProgress.progress')
  })
})
