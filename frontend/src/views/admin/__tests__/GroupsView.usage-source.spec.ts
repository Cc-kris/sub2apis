import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../GroupsView.vue'), 'utf8')

describe('GroupsView usage source', () => {
  it('renders explicit realtime/degraded source instead of treating fallback as zero', () => {
    expect(source).toContain('data-testid="group-usage-source"')
    expect(source).toContain("source === \"realtime_bootstrapping\"")
    expect(source).toContain("source === \"degraded\"")
    expect(source).toContain('group_usage_source: item.group_usage_source')
  })
})
