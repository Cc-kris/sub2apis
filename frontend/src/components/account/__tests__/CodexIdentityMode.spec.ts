import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const createSource = readFileSync(resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'), 'utf8')
const editSource = readFileSync(resolve(process.cwd(), 'src/components/account/EditAccountModal.vue'), 'utf8')

describe('Codex identity mode controls', () => {
  it('exposes the four OAuth modes on create and edit forms', () => {
    for (const source of [createSource, editSource]) {
      expect(source).toContain('codexIdentityModeOptions')
      expect(source).toContain("{ value: 'disabled'")
      expect(source).toContain("{ value: 'device'")
      expect(source).toContain("{ value: 'session'")
      expect(source).toContain("{ value: 'full'")
      expect(source).toContain('data-testid="codex-identity-mode-select"')
    }
  })

  it('only persists the mode for OpenAI OAuth accounts and removes disabled values', () => {
    expect(createSource).toContain("accountCategory.value === 'oauth-based' && codexIdentityMode.value !== 'disabled'")
    expect(createSource).toContain('delete extra.codex_identity_mode')
    expect(editSource).toContain("props.account.type === 'oauth'")
    expect(editSource).toContain('delete newExtra.codex_identity_mode')
  })

  it('normalizes unknown stored values to disabled on edit', () => {
    expect(editSource).toContain("identityMode === 'device' || identityMode === 'session' || identityMode === 'full'")
    expect(editSource).toContain(": 'disabled'")
  })
})
