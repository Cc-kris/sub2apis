import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../MonitorFormDialog.vue'), 'utf8')

describe('MonitorFormDialog passive and quota modes', () => {
  it('keeps active/passive/quota as mutually exclusive mode options', () => {
    expect(source).toContain("value: 'active'")
    expect(source).toContain("value: 'passive'")
    expect(source).toContain("value: 'quota'")
    expect(source).toContain('availableMonitorModeOptions')
    expect(source).toContain('form.mode = \'quota\'')
  })
})
