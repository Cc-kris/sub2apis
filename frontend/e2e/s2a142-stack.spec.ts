import { test, expect } from '@playwright/test'

test('TASK-040B exposes the fixed browser project infrastructure', ({}, testInfo) => {
  expect([
    'chromium',
    'tablet-chrome',
    'mobile-chrome',
    'webkit-desktop',
    'webkit-mobile'
  ]).toContain(testInfo.project.name)
})
