import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../GroupsView.vue'),
  'utf8'
)

describe('GroupsView model pricing contract', () => {
  it('exposes long-context pricing and validates interval JSON before save', () => {
    expect(source).toContain('v-model="createForm.long_context_pricing_enabled"')
    expect(source).toContain('v-model="createForm.model_pricing_json"')
    expect(source).toContain('parseGroupModelPricing(createForm.model_pricing_json)')
    expect(source).toContain('parseGroupModelPricing(editForm.model_pricing_json)')
  })
})
