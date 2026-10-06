import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { test } from 'node:test';
import { chromium } from 'playwright-core';

const defaultChrome = '/usr/bin/google-chrome';

async function waitForLayout(page) {
  await page.evaluate(() => new Promise((resolve) => {
    requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 100)));
  }));
}

async function routeAliases(page) {
  return page.locator('.route-card input[data-field="alias"]').evaluateAll((inputs) => inputs.map((input) => input.value));
}

async function waitForPolicyReady(page) {
  await page.waitForFunction(() => document.querySelectorAll('#policy-rules .policy-rule').length > 0, { timeout: 20_000 });
}

test('dashboard tabs retain their browser interactions after source assembly', { timeout: 120_000 }, async () => {
  const dashboardUrl = process.env.DASHBOARD_URL;
  const managementKey = process.env.DASHBOARD_MANAGEMENT_KEY;
  const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || defaultChrome;

  assert.ok(dashboardUrl, 'DASHBOARD_URL must point to a disposable local CPA instance');
  assert.ok(managementKey, 'DASHBOARD_MANAGEMENT_KEY must be set for that instance');
  assert.equal(process.env.DASHBOARD_E2E_ALLOW_MUTATIONS, '1', 'set DASHBOARD_E2E_ALLOW_MUTATIONS=1 only for an isolated test instance');
  assert.ok(existsSync(executablePath), `Chromium executable does not exist: ${executablePath}`);

  const parsedUrl = new URL(dashboardUrl);
  assert.ok(['127.0.0.1', 'localhost', '::1'].includes(parsedUrl.hostname), 'DASHBOARD_URL must use loopback');

  const browser = await chromium.launch({
    executablePath,
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  const pageErrors = [];
  const failedApiResponses = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  page.on('response', (response) => {
    if (response.url().includes('/v0/management/') && response.status() >= 400) {
      failedApiResponses.push(`${response.status()} ${new URL(response.url()).pathname}`);
    }
  });

  try {
    await page.goto(dashboardUrl, { waitUntil: 'domcontentloaded' });
    await page.locator('#management-key').fill(managementKey);
    await page.locator('#connect').click();
    await page.locator('#workspace').waitFor({ state: 'visible' });
    await page.locator('#model-status[data-tone="ready"]').waitFor();

    assert.equal(await page.locator('[role="tab"]').count(), 2);
    assert.equal(await page.locator('#configuration-tab').getAttribute('aria-selected'), 'true');
    assert.deepEqual(await routeAliases(page), [], 'the disposable E2E instance should start without routes');

    for (let index = 0; index < 5; index++) {
      await page.locator('#add-route').click();
      let card = page.locator('.route-card').nth(index);
      await card.locator('input[data-field="alias"]').fill(`dashboard-e2e-${index}`);
      if (index === 0) {
        await card.locator('select[data-field="strategy"]').selectOption('round-robin');
        card = page.locator('.route-card').nth(index);
      }
      await card.locator('select[data-target-field="model"]').selectOption('dashboard-e2e-model-a');
      if (index === 0) await card.locator('input[data-target-field="weight"]').fill('3');
    }
    const modelOptions = await page.locator('.route-card').first().locator('select[data-target-field="model"] option').evaluateAll((options) => options.map((option) => option.value));
    assert.ok(modelOptions.includes('dashboard-e2e-model-a'));
    assert.ok(modelOptions.includes('dashboard-e2e-model-b'));

    let firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('button[data-action="add-target"]').click();
    firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('.target-row').nth(1).locator('select[data-target-field="model"]').selectOption('dashboard-e2e-model-b');
    await firstRoute.locator('.target-row').nth(1).locator('input[data-target-field="weight"]').fill('1');
    await firstRoute.locator('.target-row').nth(1).locator('input[data-target-field="cooldown_seconds"]').fill('15');
    await firstRoute.locator('.target-row').nth(1).locator('button[data-action="target-up"]').click();
    firstRoute = page.locator('.route-card').first();
    assert.deepEqual(await firstRoute.locator('select[data-target-field="model"]').evaluateAll((selects) => selects.map((select) => select.value)), [
      'dashboard-e2e-model-b',
      'dashboard-e2e-model-a',
    ]);
    await firstRoute.locator('.target-row').first().locator('button[data-action="target-down"]').click();

    const beforeAliases = Array.from({ length: 5 }, (_, index) => `dashboard-e2e-${index}`);
    const moveUp = page.locator('.route-card[data-index="3"] button[data-action="up"]');
    await moveUp.scrollIntoViewIfNeeded();
    await waitForLayout(page);
    const scrollBefore = await page.evaluate(() => window.scrollY);
    await moveUp.click();
    await waitForLayout(page);
    const scrollAfter = await page.evaluate(() => window.scrollY);
    assert.ok(Math.abs(scrollBefore - scrollAfter) <= 1, `route reorder changed scrollY from ${scrollBefore} to ${scrollAfter}`);
    assert.deepEqual(await routeAliases(page), [beforeAliases[0], beforeAliases[1], beforeAliases[3], beforeAliases[2], beforeAliases[4]]);
    assert.equal(await page.evaluate(() => document.activeElement?.dataset.action), 'up');
    await page.keyboard.press('Enter');
    await waitForLayout(page);
    assert.deepEqual(await routeAliases(page), [beforeAliases[0], beforeAliases[3], beforeAliases[1], beforeAliases[2], beforeAliases[4]]);
    await page.locator('.route-card[data-index="1"] button[data-action="down"]').click();
    await page.locator('.route-card[data-index="2"] button[data-action="down"]').click();
    assert.deepEqual(await routeAliases(page), beforeAliases);

    await page.locator('#add-route').click();
    await page.locator('.route-card').last().locator('button[data-action="remove"]').click();
    assert.deepEqual(await routeAliases(page), beforeAliases);

    // The error policy tab mirrors the built-in table when the plugin has no
    // error_policy configured, so the panel is never an empty form.
    await page.locator('#policy-tab').click();
    await page.locator('#policy-panel').waitFor({ state: 'visible' });
    await waitForPolicyReady(page);
    for (const selector of ['#policy-rules', '#policy-default-action', '#policy-default-cooldown', '#policy-add-rule', '#attempt-timeout']) {
      assert.equal(await page.locator(selector).count(), 1, `${selector} should be present`);
    }
    const ruleCountBefore = await page.locator('#policy-rules .policy-rule').count();
    await page.locator('#policy-add-rule').click();
    assert.equal(await page.locator('#policy-rules .policy-rule').count(), ruleCountBefore + 1);
    const newRule = page.locator('#policy-rules .policy-rule').last();
    await newRule.locator('[data-policy-field="status"]').fill('429');
    await newRule.locator('[data-policy-field="cooldown_seconds"]').fill('20');
    await newRule.locator('[data-policy-field="honor_retry_after"]').check();
    await newRule.locator('[data-policy-field="backoff"]').check();
    assert.equal(await newRule.locator('[data-policy-field="cooldown_seconds"]').inputValue(), '20');
    await newRule.locator('button[data-action="remove-rule"]').click();
    assert.equal(await page.locator('#policy-rules .policy-rule').count(), ruleCountBefore);

    await page.locator('#configuration-tab').click();
    await page.locator('#save').click();
    await page.waitForFunction(() => document.querySelector('#save-state')?.textContent === 'Đã lưu vào CPA', { timeout: 20_000 });

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.locator('#policy-tab').click();
    assert.equal(await page.locator('#policy-panel').isVisible(), true);
    await page.locator('#configuration-tab').click();
    assert.equal(await page.locator('#configuration-panel').isVisible(), true);

    assert.deepEqual(pageErrors, [], 'dashboard should not raise browser runtime errors');
    assert.deepEqual(failedApiResponses, [], 'dashboard management requests should succeed');
  } finally {
    await browser.close();
  }
});
