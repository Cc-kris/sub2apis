import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'),
  'utf8'
)

describe('CreateAccountModal domestic API-key providers', () => {
  it('never routes Kimi, Zhipu, or DeepSeek through the OAuth step', () => {
    expect(source).toContain("if (isDomesticOpenAICompatiblePlatform(form.platform)) {")
    expect(source).toContain("return false\n  }\n  // Antigravity upstream 类型不需要 OAuth 流程")
  })

  it('renders and submits API-key credentials even before platform watcher flushes', () => {
    expect(source).toContain(
      "(form.type === 'apikey' || isDomesticOpenAICompatiblePlatform(form.platform)) && form.platform !== 'antigravity'"
    )
    expect(source).toContain("accountCategory.value = 'apikey'\n    form.type = 'apikey'")
  })
})
