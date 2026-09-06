import { expect, test } from '@playwright/test'
import { writeFile } from 'node:fs/promises'

const hasAdminCredentials = Boolean(process.env.E2E_ADMIN_EMAIL && process.env.E2E_ADMIN_PASSWORD)

test.describe('管理员批量编辑账号', () => {
  test.describe.configure({ mode: 'serial' })

  test.skip(!hasAdminCredentials, 'SKIP_NO_CREDENTIAL: requires the isolated real service admin seed')

  test.beforeEach(async ({ page, request, baseURL }) => {

    const login = await request.post(`${baseURL}/api/v1/auth/login`, {
      data: {
        email: process.env.E2E_ADMIN_EMAIL,
        password: process.env.E2E_ADMIN_PASSWORD
      }
    })
    expect(login.ok()).toBeTruthy()
    const payload = await login.json()
    const auth = payload.data
    await page.goto('/login')
    await page.evaluate(({ auth }) => {
      localStorage.setItem('auth_token', auth.access_token)
      localStorage.setItem('auth_user', JSON.stringify(auth.user))
      localStorage.setItem('refresh_token', auth.refresh_token)
      localStorage.setItem('token_expires_at', String(Date.now() + auth.expires_in * 1000))
      localStorage.setItem('sub2api_locale', 'zh')
      localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
    }, { auth })
  })

  test('真实服务批量提交失败时保留已填写字段', async ({ page }, testInfo) => {
    await page.goto('/admin/accounts')
    const accountName = /s2a142_.*oauth-account/
    let row = page.locator('tr[data-row-id]').filter({ hasText: accountName }).first()
    await expect(row).toBeVisible({ timeout: 20_000 })

    const accountID = await row.getAttribute('data-row-id')
    expect(accountID).toBeTruthy()
    const authToken = await page.evaluate(() => localStorage.getItem('auth_token'))
    const reset = await page.request.put(`/api/v1/admin/accounts/${accountID}`, {
      headers: { Authorization: `Bearer ${authToken}` },
      data: { type: 'oauth', credentials: { access_token: 'fixture' } }
    })
    expect(reset.ok()).toBeTruthy()
    await page.reload()
    row = page.locator('tr[data-row-id]').filter({ hasText: accountName }).first()
    await expect(row).toBeVisible({ timeout: 20_000 })
    await row.locator('input[type="checkbox"]').check()
    await page.getByTestId('bulk-edit-selected').click()

    const dialog = page.getByRole('dialog').filter({ has: page.locator('#bulk-edit-account-form') })
    await expect(dialog).toBeVisible()
    await dialog.locator('#bulk-edit-openai-codex-identity-enabled').check()
    await dialog.locator('[data-testid="bulk-edit-openai-codex-identity-mode-select"] button').click()
    await page.getByRole('option').filter({ hasText: /session|会话/i }).first().click()
    await dialog.locator('#bulk-edit-base-url-enabled').check()
    const baseURLInput = dialog.locator('#bulk-edit-base-url')
    await baseURLInput.fill('https://retry.example.invalid')

    const typeChange = await page.request.put(`/api/v1/admin/accounts/${accountID}`, {
      headers: { Authorization: `Bearer ${authToken}` },
      data: { type: 'apikey', credentials: { api_key: 'fixture' } }
    })
    expect(typeChange.ok()).toBeTruthy()

    const responsePromise = page.waitForResponse(response =>
      response.url().includes('/api/v1/admin/accounts/bulk-update')
    )
    await page.locator('button[type="submit"][form="bulk-edit-account-form"]').click()
    const response = await responsePromise
    expect(response.status()).toBe(422)

    const persistedResponse = await page.request.get(`/api/v1/admin/accounts/${accountID}`, {
      headers: { Authorization: `Bearer ${authToken}` }
    })
    expect(persistedResponse.ok()).toBeTruthy()
    const persistedPayload = await persistedResponse.json()
    const persisted = persistedPayload.data
    expect(persisted.type).toBe('apikey')
    expect(persisted.credentials?.base_url).not.toBe('https://retry.example.invalid')
    expect(persisted.extra?.codex_identity_mode).not.toBe('session')

    await expect(dialog).toBeVisible()
    await expect(baseURLInput).toHaveValue('https://retry.example.invalid')
    const screenshot = await page.screenshot()
    if (process.env.E2E_EVIDENCE_SCREENSHOT) {
      await writeFile(process.env.E2E_EVIDENCE_SCREENSHOT, screenshot)
    }
    await testInfo.attach('accounts-bulk-edit-422', {
      body: screenshot,
      contentType: 'image/png'
    })
  })
})
