import { test, expect } from '@playwright/test'
import { login, CATALOG, MONGO_URI } from './helpers'

// These specs run against a lakesync instance whose admin credentials are in
// helpers.ts. The first-run spec needs an EMPTY app (fresh database): it is
// skipped automatically when connections already exist.

test.describe('first-run setup to streaming data', () => {
  test('guided flow: MongoDB -> destination -> pipeline', async ({ page }) => {
    await login(page)
    const overview = await page.request.get('/api/overview')
    const data = await overview.json()
    test.skip(!data.first_run, 'the app already has connections; the first-run flow only shows on an empty app')
    await page.goto('/')
    await page.getByRole('button', { name: /Start setup/ }).click()
    await expect(page).toHaveURL(/\/setup/)
    // step 1: MongoDB via paste + parse
    await page.getByLabel('Name', { exact: true }).fill('Local MongoDB')
    await page.locator('#paste').fill(MONGO_URI)
    await page.getByRole('button', { name: 'Parse' }).click()
    await expect(page.locator('#f-uri')).toHaveValue(/mongodb:\/\//)
    await page.getByRole('button', { name: 'Test connection' }).click()
    await expect(page.getByText('Open a change stream')).toBeVisible({ timeout: 60000 })
    await expect(page.getByText('All checks passed')).toBeVisible()
    await page.getByRole('button', { name: 'Save', exact: true }).click()
    // step 2: destination
    await expect(page.getByText('Add an Iceberg destination')).toBeVisible()
    await page.getByLabel('Name', { exact: true }).fill('Local Nessie')
    await page.locator('#f-catalog_uri').fill(CATALOG.catalog_uri)
    await page.locator('#f-warehouse').fill(CATALOG.warehouse)
    await page.locator('#f-s3_endpoint').fill(CATALOG.s3_endpoint)
    await page.locator('#f-s3_access_key_id').fill(CATALOG.key)
    await page.locator('#f-s3_secret_access_key').fill(CATALOG.secret)
    await page.getByRole('button', { name: 'Test connection' }).click()
    await expect(page.getByText('DuckDB attaches the catalog')).toBeVisible({ timeout: 90000 })
    await page.getByRole('button', { name: 'Save', exact: true }).click()
    // step 3: pipeline
    await expect(page.getByText('Create your first pipeline')).toBeVisible()
    await page.getByRole('button', { name: 'Next' }).click()
    await expect(page.getByText(/collections/).first()).toBeVisible({ timeout: 60000 })
    // pick the seeded "shop" database (other databases may exist on a shared MongoDB)
    await page.getByPlaceholder('Filter databases and collections').fill('shop.')
    const shopDb = page.locator('label', { hasText: /^shop/ }).first()
    await expect(shopDb).toBeVisible({ timeout: 30000 })
    await shopDb.locator('button[role=checkbox]').first().click()
    await page.getByRole('button', { name: 'Next' }).click()
    await page.getByRole('button', { name: 'Next' }).click()
    await page.getByRole('button', { name: /Create and start/ }).click()
    await expect(page).toHaveURL(/\/pipelines\//)
    await expect(page.getByText('Streaming').first()).toBeVisible({ timeout: 180000 })
  })
})

test.describe('data viewer', () => {
  test('filters, search, JSON panel and document history', async ({ page }) => {
    await login(page)
    await page.goto('/tables')
    const link = page.locator('a[href*="/tables/"][href*="customers?"]').first()
    const present = await link.waitFor({ state: 'visible', timeout: 20000 }).then(() => true, () => false)
    test.skip(!present, 'needs a synced customers table')
    await link.click()
    await expect(page.locator('[data-row="0"]')).toBeVisible({ timeout: 30000 })
    const before = await page.getByText(/rows/).first().innerText()
    // full-text search narrows the rows
    await page.getByPlaceholder('Search all text columns').fill('Lisbon')
    await expect(page.getByText(/rows match/)).toBeVisible({ timeout: 30000 })
    const after = await page.getByText(/rows match/).innerText()
    expect(after).not.toEqual(before)
    await page.getByPlaceholder('Search all text columns').fill('')
    // a column filter
    await page.getByRole('button', { name: 'Add filter' }).click()
    await page.getByRole('combobox').first().click()
    await page.getByRole('option', { name: /active/ }).click()
    await page.getByRole('button', { name: 'Apply' }).click()
    await expect(page.getByText(/active equals/)).toBeVisible()
    await expect(page.getByText(/rows match/)).toBeVisible({ timeout: 30000 })
    // sort by name (the header cell carries a "name (type)" title)
    await page.locator('[title^="name ("]').first().click({ force: true })
    await expect(page.locator('[data-row="0"]')).toBeVisible({ timeout: 30000 })
    // JSON side panel from a nested column
    await page.locator('[data-row="0"]').getByText(/^(\{ \}|\[ \])$/).first().click()
    await expect(page.getByText('Nested value stored as JSON text')).toBeVisible()
    await page.keyboard.press('Escape')
    // document detail with change history
    await page.locator('[data-row="0"]').click()
    await page.getByRole('tab', { name: /Change history/ }).click()
    await expect(page.getByText(/seq \d+/).first()).toBeVisible({ timeout: 30000 })
    await page.keyboard.press('Escape')
    // raw toggle
    await page.getByRole('button', { name: 'Raw', exact: true }).click()
    await expect(page.getByText('raw events')).toBeVisible()
  })
})

test.describe('sql console', () => {
  test('runs a SELECT and rejects a write', async ({ page }) => {
    await login(page)
    const dests = await (await page.request.get('/api/connections?kind=destination')).json()
    test.skip(!dests.length, 'needs a destination connection')
    await page.goto('/sql')
    await expect(page.locator('.cm-content')).toBeVisible()
    await page.locator('.cm-content').click()
    await page.keyboard.press('Meta+a')
    await page.keyboard.type('SELECT 1 AS one, 2 AS two')
    await page.getByRole('button', { name: /Run/ }).click()
    await expect(page.getByTitle(/^one \(/)).toBeVisible({ timeout: 30000 })
    await expect(page.getByText(/^1 row/).first()).toBeVisible()
    await page.locator('.cm-content').click()
    await page.keyboard.press('Meta+a')
    await page.keyboard.type('INSERT INTO shop.customers VALUES (1)')
    await page.getByRole('button', { name: /Run/ }).click()
    await expect(page.getByText(/only SELECT and EXPLAIN/)).toBeVisible({ timeout: 30000 })
  })
})

test.describe('pipeline controls', () => {
  test('pause and resume', async ({ page }) => {
    await login(page)
    await page.goto('/pipelines')
    const card = page.locator('a[href^="/pipelines/"]:not([href$="/new"])').first()
    const present = await card.waitFor({ state: 'visible', timeout: 20000 }).then(() => true, () => false)
    test.skip(!present, 'needs a pipeline')
    await card.click()
    // The detail page renders Pause while running and Start/Resume otherwise; wait for either.
    const control = page.getByRole('button', { name: /^(Pause|Resume|Start)$/ }).first()
    await control.waitFor({ state: 'visible', timeout: 30000 })
    const pause = page.getByRole('button', { name: 'Pause' })
    if (await pause.count()) {
      await pause.click()
      await expect(page.getByText('Pipeline paused')).toBeVisible()
      await expect(page.getByText('Paused').first()).toBeVisible({ timeout: 60000 })
      await page.getByRole('button', { name: /Resume|Start/ }).click()
      await expect(page.getByText('Streaming').first()).toBeVisible({ timeout: 120000 })
    } else {
      await page.getByRole('button', { name: /Start|Resume/ }).click()
      await expect(page.getByText('Streaming').first()).toBeVisible({ timeout: 120000 })
    }
  })
})

test.describe('connection test hints', () => {
  test('a wrong MongoDB host shows a plain-English hint', async ({ page }) => {
    await login(page)
    await page.goto('/connections')
    await page.getByRole('button', { name: 'Source' }).click()
    await page.getByLabel('Name', { exact: true }).fill('Broken')
    await page.locator('#f-uri').fill('mongodb://does-not-exist.invalid:27017/?directConnection=true&serverSelectionTimeoutMS=2000&connectTimeoutMS=2000')
    await page.getByRole('button', { name: 'Test connection' }).click()
    await expect(page.getByText('Some checks failed')).toBeVisible({ timeout: 60000 })
    await expect(page.getByText(/DNS|resolve|host/i).first()).toBeVisible()
    await expect(page.getByRole('button', { name: 'Save', exact: true })).toBeVisible()
  })
})
