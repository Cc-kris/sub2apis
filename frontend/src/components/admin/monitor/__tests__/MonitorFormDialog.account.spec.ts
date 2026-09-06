import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../MonitorFormDialog.vue'), 'utf8')

describe('MonitorFormDialog account search', () => {
  it('uses a debounced search field and writes the selected account id', () => {
    expect(source).toContain('type="search"')
    expect(source).toContain('accountSearchTimer')
    expect(source).toContain('setTimeout(async () => {')
    expect(source).toContain('form.account_id = account.id')
  })
})
