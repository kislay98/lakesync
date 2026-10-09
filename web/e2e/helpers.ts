import { expect, type Page } from '@playwright/test'

export const ADMIN = { email: process.env.LAKESYNC_ADMIN_EMAIL || 'admin@example.com', password: process.env.LAKESYNC_ADMIN_PASSWORD || 'changeme-now' }
export const MONGO_URI = process.env.LAKESYNC_E2E_MONGODB_URI || 'mongodb://localhost:27017/?directConnection=true'
export const CATALOG = {
  catalog_uri: process.env.LAKESYNC_E2E_CATALOG_URI || 'http://localhost:19120/iceberg',
  warehouse: 'warehouse',
  s3_endpoint: process.env.LAKESYNC_E2E_S3_ENDPOINT || 'http://localhost:9000',
  key: 'lakesync',
  secret: 'lakesync-secret',
}

export async function login(page: Page) {
  await page.goto('/login')
  await page.getByLabel('Email').fill(ADMIN.email)
  await page.getByLabel('Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).not.toHaveURL(/\/login/)
}

export async function setTheme(page: Page, theme: 'light' | 'dark') {
  await page.evaluate((t) => {
    localStorage.setItem('lakesync.theme', t)
    document.documentElement.classList.toggle('dark', t === 'dark')
  }, theme)
}
