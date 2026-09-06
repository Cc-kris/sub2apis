import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'),
  'utf8'
)

describe('CreateAccountModal Grok account types', () => {
  it('requires a procurement multiplier and preserves four-decimal text on create', () => {
    expect(source).toContain('data-testid="upstream-cost-multiplier"')
    expect(source).toContain("upstream_cost_multiplier: '1.0000'")
    expect(source).toContain('upstream_cost_multiplier: upstreamMultiplierText()')
    expect(source).toContain('isValidUpstreamMultiplier(form.upstream_cost_multiplier)')
  })

  it('offers API-key setup alongside OAuth with the official xAI default', () => {
    expect(source).toContain('data-testid="grok-account-type-api-key"')
    expect(source).toContain("@click=\"accountCategory = 'apikey'\"")
    expect(source).toContain("newPlatform === 'grok'")
    expect(source).toContain("? 'https://api.x.ai/v1'")
    expect(source).toContain("form.platform === 'grok'")
    expect(source).toContain("? 'xai-...'")
    expect(source).toContain("case 'grok':")
    expect(source).toContain("return 'https://api.x.ai/v1'")
    expect(source).toContain('apiKeyBaseUrl.value = defaultAPIKeyBaseURL(newPlatform)')
    expect(source).toContain('const defaultBaseUrl = defaultAPIKeyBaseURL(form.platform)')
  })

  it('forces domestic OpenAI-compatible providers onto the API-key flow with their own default URLs', () => {
    expect(source).toContain('const isDomesticOpenAICompatiblePlatform')
    expect(source).toContain("platform === 'kimi' || platform === 'zhipu' || platform === 'deepseek'")
    expect(source).toContain("case 'kimi':")
    expect(source).toContain("return 'https://api.moonshot.cn'")
    expect(source).toContain("case 'zhipu':")
    expect(source).toContain("return 'https://open.bigmodel.cn/api/paas'")
    expect(source).toContain("case 'deepseek':")
    expect(source).toContain("return 'https://api.deepseek.com'")
    expect(source).toContain("} else if (isDomesticOpenAICompatiblePlatform(newPlatform)) {")
    expect(source).toContain("accountCategory.value = 'apikey'")
    expect(source).toContain('const defaultBaseUrl = defaultAPIKeyBaseURL(form.platform)')
  })

  it('exposes custom upstream URL and header override for the OAuth create flow', () => {
    expect(source).toContain('data-testid="grok-custom-base-url-toggle"')
    expect(source).toContain('data-testid="grok-custom-base-url-input"')
    expect(source).toContain('form.platform === \'grok\' && isOAuthFlow')
  })

  it('validates and applies upstream config on all three Grok OAuth create paths', () => {
    // 授权码兑换 / RT 批量 / SSO 批量 3 处调用（定义为箭头函数，不计入）
    expect(source.match(/validateGrokOAuthUpstreamConfig\(\)/g)?.length).toBe(3)
    expect(source.match(/applyGrokOAuthUpstreamConfig\(credentials\)/g)?.length).toBe(3)
  })
})
