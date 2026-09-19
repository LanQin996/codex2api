import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { chromium } from 'playwright'
import { startGalleryFixture } from './fixtures/qualityTestGallery.mjs'

let fixture, browser
before(async () => {
  fixture = await startGalleryFixture()
  browser = await chromium.launch({ headless: true })
})
after(async () => {
  await browser?.close()
  await fixture?.server.close()
})
async function openPage() {
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1000 },
  })
  await page.addInitScript(() => {
    if (window.top !== window) return
    localStorage.setItem('lang', 'en')
    localStorage.setItem('admin_key', 'fixture-key')
    localStorage.setItem('codex2api:first_setup_review_done_v1', '1')
  })
  await page.goto(fixture.url + '/admin/quality-test')
  await page.locator('.quality-test-result-card').first().waitFor()
  return page
}
test('default gallery lazy-loads isolated previews, supports history and narrow layouts', async () => {
  const page = await openPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(e.message))
  try {
    assert.equal(await page.locator('.quality-test-result-card').count(), 20)
    await page.locator('.quality-test-card-preview iframe').first().waitFor()
    assert.ok(
      fixture.detailRequests < 20,
      'must not load every offscreen payload',
    )
    assert.ok(
      fixture.peak <= 3,
      'at most three concurrent preview detail requests',
    )
    assert.equal(
      await page
        .locator('.quality-test-card-preview iframe')
        .first()
        .getAttribute('sandbox'),
      'allow-scripts',
    )
    const first = page.locator('.quality-test-result-card').first()
    await first
      .getByRole('button', { name: 'View result', exact: true })
      .click()
    await page.getByRole('dialog').waitFor()
    await page.keyboard.press('Escape')
    await page.setViewportSize({ width: 390, height: 844 })
    await page.waitForTimeout(100)
    assert.ok(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      'no horizontal page overflow',
    )
    const initial = await page
      .locator('.quality-test-result-card')
      .first()
      .boundingBox()
    const second = await page
      .locator('.quality-test-result-card')
      .nth(1)
      .boundingBox()
    assert.ok(second.y > initial.y + initial.height - 1)
    assert.deepEqual(errors, [])
  } finally {
    await page.close()
  }
})

test('batch sheet stays inside the viewport with a fixed header and submit footer', async () => {
  const page = await openPage()
  try {
    for (const viewport of [{ width: 1293, height: 822 }, { width: 860, height: 548 }, { width: 390, height: 640 }]) {
      await page.setViewportSize(viewport)
      await page.getByRole('button', { name: 'Batch test', exact: true }).click()
      const panel = page.getByRole('dialog')
      await panel.getByRole('checkbox').first().waitFor()
      const submit = panel.getByRole('button', { name: 'Confirm and queue tests', exact: true })
      const bounds = await panel.boundingBox()
      assert.ok(bounds.x >= 0 && bounds.y >= 0)
      assert.ok(bounds.x + bounds.width <= viewport.width + 1)
      assert.ok(bounds.y + bounds.height <= viewport.height + 1)
      const footerBefore = await submit.boundingBox()
      assert.ok(footerBefore.y >= 0 && footerBefore.y + footerBefore.height <= viewport.height)
      await panel.locator('[data-slot="sheet-body"]').evaluate(el => { el.scrollTop = el.scrollHeight })
      const footerAfter = await submit.boundingBox()
      assert.equal(footerBefore.y, footerAfter.y)
      assert.ok(await panel.getByRole('heading', { name: 'Batch test' }).isVisible())
      assert.ok(await panel.evaluate(el => el.scrollWidth <= el.clientWidth))
      await page.keyboard.press('Escape')
      await panel.waitFor({ state: 'hidden' })
    }
  } finally {
    await page.close()
  }
})
test('batch selection spans pages, queues once, survives reload and renders completed previews', async () => {
  const page = await openPage()
  try {
    await page.getByRole('button', { name: 'Batch test', exact: true }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByRole('checkbox').first().waitFor()
    await dialog
      .getByRole('button', { name: 'Select this page', exact: true })
      .click()
    await dialog.getByRole('button', { name: '2', exact: true }).click()
    await dialog.getByRole('checkbox', { name: /Account 021/ }).waitFor()
    await dialog.getByRole('checkbox').first().check()
    await dialog.getByText('21 selected', { exact: true }).waitFor()
    await dialog.getByPlaceholder('Search account name or email').fill('125')
    await dialog.getByRole('checkbox', { name: /Account 125/ }).waitFor()
    await dialog.getByRole('checkbox').check()
    await dialog.getByText('22 selected', { exact: true }).waitFor()
    const submit = dialog.getByRole('button', {
      name: 'Confirm and queue tests',
      exact: true,
    })
    await submit.click()
    await dialog.waitFor({ state: 'hidden' })
    assert.equal(fixture.submissions.length, 1)
    assert.equal(fixture.submissions[0].account_ids.length, 22)
    assert.ok(fixture.submissions[0].account_ids.includes(125))
    await page
      .locator('.quality-test-batch-progress')
      .getByText('22', { exact: true })
      .waitFor()
    await page.reload()
    await page
      .locator('.quality-test-batch-progress')
      .getByText('22', { exact: true })
      .waitFor()
    assert.equal(fixture.submissions.length, 1)
    fixture.complete()
    await page
      .locator('.quality-test-result-card')
      .first()
      .getByText('Completed', { exact: true })
      .waitFor({ timeout: 12000 })
    await page.locator('.quality-test-card-preview iframe').first().waitFor()
  } finally {
    await page.close()
  }
})

test('batch refuses incompatible selections and exposes cancellation', async () => {
 const page = await openPage()
 try {
  await page.route('**/quality-test-batches/options', route => route.fulfill({ json: { models: [], reasoning_efforts: ['high'] } }))
  await page.getByRole('button', { name: 'Batch test', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('checkbox').first().check()
  await dialog.getByText('No common model or reasoning effort. Adjust the selected accounts.').waitFor()
  assert.equal(await dialog.getByRole('button', { name: 'Confirm and queue tests', exact: true }).isDisabled(), true)
  await page.unroute('**/quality-test-batches/options')
  await dialog.getByRole('checkbox').nth(1).check()
  await dialog.getByRole('button', { name: 'Confirm and queue tests', exact: true }).click()
  await dialog.waitFor({ state: 'hidden' })
  await page.getByRole('button', { name: 'Cancel remaining tasks', exact: true }).click()
  await page.locator('.quality-test-batch-progress').getByText('2', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Cancel remaining tasks', exact: true }).waitFor({ state: 'hidden' })
 } finally { await page.close() }
})
