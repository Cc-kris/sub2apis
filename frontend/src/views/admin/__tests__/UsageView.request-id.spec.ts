import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../UsageView.vue'), 'utf8')

describe('UsageView request ID', () => {
  it('keeps request ID as an optional column and routes error inspection by request ID', () => {
    expect(source).toContain("key: 'request_id'")
    expect(source).toContain('request_id ||')
    expect(source).toContain("query: { request_id: requestId }")
  })
})
