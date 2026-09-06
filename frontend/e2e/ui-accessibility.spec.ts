import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page, type TestInfo } from '@playwright/test'
import { mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'

async function captureUiEvidence(page: Page, testInfo: TestInfo, testName: string) {
  const evidenceDir = process.env.E2E_UI_EVIDENCE_DIR
  const screenshot = await page.screenshot()
  if (!evidenceDir) return screenshot
  await mkdir(evidenceDir, { recursive: true })
  const safeName = [testInfo.project.name, process.env.E2E_THEME || 'light', page.viewportSize()?.width, page.viewportSize()?.height, testName]
    .map(String)
    .join('_')
    .replace(/[^a-zA-Z0-9._-]+/g, '_')
  const screenshotPath = path.join(evidenceDir, `${safeName}.png`)
  await writeFile(screenshotPath, screenshot)
  const styles = await page.evaluate(() => {
    const root = document.documentElement
    const body = document.body
    const control = document.querySelector('input,button,select,textarea') as HTMLElement | null
    const style = control ? getComputedStyle(control) : null
    return {
      theme: root.className,
      viewport: { width: window.innerWidth, height: window.innerHeight },
      bodyBackground: getComputedStyle(body).backgroundColor,
      firstControl: style ? { color: style.color, backgroundColor: style.backgroundColor, outlineColor: style.outlineColor } : null
    }
  })
  const stylesJson = JSON.stringify(styles, null, 2)
  await writeFile(path.join(evidenceDir, `${safeName}.styles.json`), stylesJson)
  await testInfo.attach(`${safeName}.png`, { body: screenshot, contentType: 'image/png' })
  await testInfo.attach(`${safeName}.styles.json`, { body: stylesJson, contentType: 'application/json' })
  return screenshot
}

// This is the executable baseline for the UI task registry. Protected product
// pages are covered by their authenticated specs; the public shell is always
// reachable and is checked in every configured browser/viewport project.
test.describe('UI accessibility baseline', () => {
  test.describe.configure({ mode: 'serial' })

  test('login shell has keyboard-visible form controls and no serious violations', async ({ page }, testInfo) => {
    await page.addInitScript(theme => {
      localStorage.setItem('theme', theme)
      localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
    }, process.env.E2E_THEME || 'light')
    await page.goto('/login')
    // LoginView mounts after public settings resolve; wait for the real form control.
    await expect(page.locator('#email')).toBeVisible()
    await expect(page.locator('#email')).toBeEnabled({ timeout: 15_000 })

    const firstControl = page.locator('#email')
    await firstControl.click()
    await expect(firstControl).toBeFocused()

    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
      .analyze()
    const blocking = results.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact || ''))
    expect(blocking).toEqual([])
    await captureUiEvidence(page, testInfo, 'login-shell')
  })

  test.describe('authenticated protected pages', () => {
    test.describe.configure({ mode: 'serial' })
    test.skip(!process.env.E2E_ADMIN_EMAIL || !process.env.E2E_ADMIN_PASSWORD, 'requires isolated admin credentials')

    let cachedAuth: {
      access_token: string
      refresh_token: string
      expires_in: number
      user: unknown
    } | null = null

    test.beforeAll(async ({ request }) => {
      const email = process.env.E2E_ADMIN_EMAIL || ''
      const password = process.env.E2E_ADMIN_PASSWORD || ''
      const authBase = process.env.E2E_AUTH_BASE_URL || process.env.E2E_BASE_URL || 'http://127.0.0.1:3000'
      const login = await request.post(`${authBase}/api/v1/auth/login`, { data: { email, password } })
      expect(login.ok()).toBeTruthy()
      const payload = await login.json()
      cachedAuth = payload.data
    })

    test.beforeEach(async ({ page }) => {
      await page.addInitScript(theme => {
        localStorage.setItem('theme', theme)
        localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
      }, process.env.E2E_THEME || 'light')
      expect(cachedAuth).not.toBeNull()
      await page.goto('/login')
      await page.evaluate(({ auth }) => {
        localStorage.setItem('auth_token', auth.access_token)
        localStorage.setItem('auth_user', JSON.stringify(auth.user))
        localStorage.setItem('refresh_token', auth.refresh_token)
        localStorage.setItem('token_expires_at', String(Date.now() + auth.expires_in * 1000))
      }, { auth: cachedAuth! })
      await page.goto('/dashboard')
      await expect(page).not.toHaveURL(/\/login/)
    })

    const protectedPages = [
      { route: '/admin/settings', readySelector: '#settings-table-default-page-size' },
      { route: '/admin/announcements', readySelector: '.table-page-layout' },
      { route: '/admin/usage', readySelector: '[data-test="usage-content-ready"]' }
    ]

    for (const { route, readySelector } of protectedPages) {
      test(`${route} has no serious accessibility violations`, async ({ page }, testInfo) => {
        await page.goto(route)
        await expect(page.locator(`main ${readySelector}`).first()).toBeVisible({ timeout: 15_000 })
        const results = await new AxeBuilder({ page })
          .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
          .analyze()
        const blocking = results.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact || ''))
        expect(blocking).toEqual([])
        await captureUiEvidence(page, testInfo, route.replaceAll('/', '_'))
      })
    }
  })
})
