import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../AppHeader.vue'),
  'utf8'
)

describe('AppHeader role labels', () => {
  it('uses translated labels for known roles and a translated fallback for unknown roles', () => {
    expect(source).toContain("if (role === 'admin') return t('profile.administrator')")
    expect(source).toContain("if (role === 'user') return t('profile.user')")
    expect(source).toContain("t('profile.unknownRole', { role })")
    expect(source).toContain("return role ? t('profile.unknownRole', { role }) : t('profile.user')")
  })
})
