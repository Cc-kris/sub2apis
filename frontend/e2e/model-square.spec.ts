import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'

const user = {
  id: 42,
  username: 'model-square-user',
  email: 'model-square@example.com',
  role: 'user',
  balance: 100,
  concurrency: 5,
  status: 'active',
  created_at: '2026-07-26T00:00:00Z',
  updated_at: '2026-07-26T00:00:00Z',
}

const unitPrice = (original: string, multiplier: string, unit = 'per_1m_tokens') => ({
  original,
  multiplier_price: multiplier,
  unit,
})

async function mockAuthenticatedModelSquare(page: Page) {
  await page.addInitScript((savedUser) => {
    localStorage.setItem('auth_token', 'e2e-token')
    localStorage.setItem('auth_user', JSON.stringify(savedUser))
    localStorage.setItem('sub2api_locale', 'zh')
    localStorage.setItem('user_guide_42_user_v4_interactive', 'true')
    ;(window as any).__APP_CONFIG__ = {
      site_name: 'CCAI',
      site_logo: '',
      version: 'e2e',
      backend_mode_enabled: false,
      available_channels_enabled: true,
      model_square_enabled: true,
      channel_monitor_enabled: true,
      payment_enabled: false,
      affiliate_enabled: false,
      custom_menu_items: [],
    }
  }, user)

  await page.route('**/api/v1/**', async (route) => {
    await route.fulfill({ json: { code: 0, message: 'success', data: {} } })
  })
  await page.route('**/api/v1/auth/me*', async (route) => {
    await route.fulfill({ json: { code: 0, message: 'success', data: user } })
  })
  await page.route(/\/api\/v1\/model-square\/groups(?:\/\d+\/models)?(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url())
    const match = url.pathname.match(/groups\/(\d+)\/models$/)
    if (!match) {
      await route.fulfill({
        json: {
          code: 0,
          message: 'success',
          data: {
            groups: [
              { id: 10, name: 'OpenAI 标准组', platform: 'openai', subscription_type: 'standard', default_multiplier: '1.2000', effective_multiplier: '1.1000', has_custom_multiplier: true, model_count: 2 },
              { id: 20, name: 'Anthropic 公开组', platform: 'anthropic', subscription_type: 'standard', default_multiplier: '1.0000', effective_multiplier: '1.0000', has_custom_multiplier: false, model_count: 1 },
            ],
            catalog_updated_at: '2026-07-26T08:30:00Z',
          },
        },
      })
      return
    }

    const groupId = Number(match[1])
    const query = (url.searchParams.get('q') || '').toLowerCase()
    const openAIModels = [
      {
        name: 'gpt-5.5',
        billing_mode: 'token',
        prices: {
          input: unitPrice('2.50000000', '2.75000000'),
          output: unitPrice('15.00000000', '16.50000000'),
          cache_read: unitPrice('0.25000000', '0.27500000', 'per_1m_cache_tokens'),
          cache_write_5m: null,
          cache_write_1h: null,
        },
        fast_prices: {
          input: unitPrice('5.00000000', '5.50000000'),
          output: unitPrice('30.00000000', '33.00000000'),
          cache_read: null,
          cache_write_5m: null,
          cache_write_1h: null,
        },
        tiers: [],
      },
      {
        name: 'gpt-image-2',
        billing_mode: 'image',
        prices: {
          input: null,
          output: null,
          cache_read: null,
          cache_write_5m: null,
          cache_write_1h: null,
          image_output: unitPrice('0.04000000', '0.04400000', 'per_image'),
        },
        fast_prices: null,
        tiers: [{
          min_tokens: 0,
          max_tokens: null,
          tier_label: 'HD',
          sort_order: 0,
          input: null,
          output: null,
          cache_read: null,
          cache_write: null,
          per_request: unitPrice('0.08000000', '0.08800000', 'per_image'),
        }],
      },
    ]
    const models = groupId === 20
      ? [{
          name: 'claude-sonnet-4-6', billing_mode: 'token',
          prices: { input: unitPrice('3.00000000', '3.00000000'), output: unitPrice('15.00000000', '15.00000000'), cache_read: null, cache_write_5m: null, cache_write_1h: null },
          fast_prices: null, tiers: [],
        }]
      : openAIModels.filter(model => !query || model.name.toLowerCase().includes(query))

    await route.fulfill({
      json: {
        code: 0,
        message: 'success',
        data: {
          group_id: groupId,
          group_name: groupId === 20 ? 'Anthropic 公开组' : 'OpenAI 标准组',
          effective_multiplier: groupId === 20 ? '1.0000' : '1.1000',
          items: models,
          next_cursor: null,
          catalog_updated_at: '2026-07-26T08:30:00Z',
        },
      },
    })
  })
}

test.describe('Model Square', () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticatedModelSquare(page)
  })

  test('browses groups, prices, billing details and Fast details', async ({ page, isMobile }, testInfo) => {
    test.skip(Boolean(isMobile), 'Desktop group list workflow')
    await page.goto('/model-square')

    await expect(page.getByRole('heading', { name: '模型广场' })).toBeVisible({ timeout: 15_000 })
    await expect(page).toHaveURL(/group_id=10/)
    await expect(page.getByRole('option', { name: /OpenAI 标准组/ })).toHaveAttribute('aria-selected', 'true')
    const list = page.getByTestId('model-square-desktop-list')
    await expect(list.getByText('gpt-5.5', { exact: true })).toBeVisible()
    await expect(list.getByTitle(/倍率价 2\.75000000/)).toBeVisible()
    await expect(list.getByText(/原价 \$2\.5/)).toBeVisible()

    await list.getByText('查看 Fast 价格').click()
    await expect(list.getByTitle(/倍率价 5\.50000000/)).toBeVisible()

    await page.getByRole('searchbox', { name: '搜索当前分组的模型' }).fill('image')
    await expect(list.getByText('gpt-image-2', { exact: true })).toBeVisible()
    await expect(list.getByText('图片', { exact: true })).toBeVisible()
    await expect(list.getByTitle(/倍率价 0\.04400000/)).toBeVisible()

    await page.getByRole('option', { name: /Anthropic 公开组/ }).click()
    await expect(page).toHaveURL(/group_id=20/)
    await expect(list.getByText('claude-sonnet-4-6', { exact: true })).toBeVisible()
    await testInfo.attach('model-square-desktop', { body: await page.screenshot(), contentType: 'image/png' })
  })

  test('debounces search and has no serious accessibility violations', async ({ page, isMobile }, testInfo) => {
    await page.goto('/model-square?group_id=10')
    const search = page.getByRole('searchbox', { name: '搜索当前分组的模型' })
    await search.fill('image')
    const list = page.getByTestId(isMobile ? 'model-square-mobile-list' : 'model-square-desktop-list')
    await expect(list.getByText('gpt-image-2', { exact: true })).toBeVisible()
    await expect(page.getByText('gpt-5.5', { exact: true })).toHaveCount(0)

    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
      .analyze()
    const blocking = results.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact || ''))
    expect(blocking).toEqual([])
    await testInfo.attach('model-square-search', { body: await page.screenshot(), contentType: 'image/png' })
  })

  test('uses the mobile group drawer', async ({ page, isMobile }, testInfo) => {
    test.skip(!isMobile, 'Mobile group drawer workflow')
    await page.goto('/model-square?group_id=10')
    await page.getByRole('button', { name: /OpenAI 标准组/ }).click()
    await expect(page.getByRole('dialog', { name: '选择分组' })).toBeVisible()
    await page.getByRole('dialog').getByRole('button', { name: /Anthropic 公开组/ }).click()
    await expect(page).toHaveURL(/group_id=20/)
    await expect(page.getByTestId('model-square-mobile-list').getByText('claude-sonnet-4-6', { exact: true })).toBeVisible()
    await testInfo.attach('model-square-mobile', { body: await page.screenshot(), contentType: 'image/png' })
  })
})
