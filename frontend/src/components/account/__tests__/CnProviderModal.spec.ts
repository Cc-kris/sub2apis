import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../CreateAccountModal.vue'), 'utf8')

describe('Chinese provider account form', () => {
  it('offers Kimi, Zhipu and DeepSeek with protocol-specific base URLs', () => {
    expect(source).toContain("{ id: 'kimi', label: 'Kimi' }")
    expect(source).toContain("{ id: 'zhipu', label: '智谱' }")
    expect(source).toContain("{ id: 'deepseek', label: 'DeepSeek' }")
    expect(source).toContain('apiBaseUrls.chat_completions')
    expect(source).toContain('apiBaseUrls.anthropic')
    expect(source).toContain('apiBaseUrls.responses')
  })
})
