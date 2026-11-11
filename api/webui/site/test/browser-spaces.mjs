import assert from "node:assert/strict";

async function verifyRowEdges(row) {
  await row.hover();
  const edges = await row.evaluate(async (element) => {
    await new Promise((resolve) => requestAnimationFrame(resolve));
    const card = element.closest(".management-panel");
    const cardBox = card.getBoundingClientRect();
    const rowBox = element.getBoundingClientRect();
    const cardStyle = getComputedStyle(card);
    return {
      left: rowBox.left - cardBox.left - parseFloat(cardStyle.borderLeftWidth),
      right:
        cardBox.right - rowBox.right - parseFloat(cardStyle.borderRightWidth),
      background: getComputedStyle(element).backgroundColor,
      hovered: element.matches(":hover"),
    };
  });
  assert.ok(
    Math.abs(edges.left) <= 1,
    "Row highlight must reach the card's left edge",
  );
  assert.ok(
    Math.abs(edges.right) <= 1,
    "Row highlight must reach the card's right edge",
  );
  assert.notEqual(edges.background, "rgba(0, 0, 0, 0)", JSON.stringify(edges));
}

export async function verifySpaces({ page, origin, api }) {
  await api("/organizations/browser-team/spaces/staging", "PUT", {
    cluster: "target",
  });
  await page.goto(origin + "/?view=spaces&organization=browser-team");
  const sidebar = page.getByRole("navigation", { name: "Main navigation" });
  const branch = (name) =>
    sidebar.getByRole("group", { name: name + " space", exact: true });
  await sidebar
    .getByRole("link", { name: "All spaces", exact: true })
    .waitFor();
  await verifyRowEdges(page.locator(".application-list-item").first());
  assert.equal(
    await page.getByRole("link", { name: /^Settings for / }).count(),
    0,
  );
  assert.equal(await branch("other-space").count(), 0);
  assert.equal(
    await branch("dev")
      .getByRole("link", { name: "Applications", exact: true })
      .isVisible(),
    false,
  );
  await branch("dev").getByRole("button", { name: "Expand dev space" }).focus();
  await page.keyboard.press("Enter");
  assert.equal(
    await branch("dev").getByRole("button").getAttribute("aria-expanded"),
    "true",
  );
  await branch("dev").getByRole("link", { name: "dev", exact: true }).click();
  await page.getByRole("navigation", { name: "Space sections" }).waitFor();
  assert.equal(
    await branch("dev")
      .getByRole("link", { name: "dev", exact: true })
      .getAttribute("aria-current"),
    "page",
  );
  assert.equal(
    await branch("dev").getByRole("button").getAttribute("aria-expanded"),
    "true",
  );
  assert.equal(
    await branch("staging").getByRole("button").getAttribute("aria-expanded"),
    "false",
  );
  await branch("staging").getByRole("button").click();
  assert.equal(
    await branch("staging")
      .getByRole("link", { name: "Settings", exact: true })
      .count(),
    0,
  );
  for (const [label, view] of [
    ["Applications", "applications"],
    ["Managed addons", "postgres"],
    ["Custom addons", "custom-charts"],
    ["Secrets", "secrets"],
  ]) {
    await branch("staging")
      .getByRole("link", { name: label, exact: true })
      .click();
    await page.waitForURL(
      (url) =>
        url.searchParams.get("view") === view &&
        url.searchParams.get("space") === "staging",
    );
    assert.equal(
      await branch("staging")
        .getByRole("link", { name: label, exact: true })
        .getAttribute("aria-current"),
      "page",
    );
    assert.equal(
      await branch("dev").getByRole("button").getAttribute("aria-expanded"),
      "true",
      "Opening a section in staging must keep dev expanded",
    );
    assert.equal(
      new URL(page.url()).searchParams.get("organization"),
      "browser-team",
    );
  }
  await page.goto(
    origin + "/?view=space-settings&organization=browser-team&space=staging",
  );
  await page.waitForURL((url) => url.searchParams.get("view") === "secrets");
  await page
    .getByRole("heading", { name: "Secrets in staging", exact: true })
    .waitFor();
  await branch("dev").getByRole("link", { name: "dev", exact: true }).click();
  await page.waitForURL((url) => url.searchParams.get("space") === "dev");
  assert.equal(
    await branch("staging").getByRole("button").getAttribute("aria-expanded"),
    "true",
    "Opening dev must keep staging expanded",
  );
  const tabs = page.getByRole("navigation", { name: "Space sections" });
  const metricsRequest = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return (
      url.pathname === "/api/ui/space" &&
      url.searchParams.get("panel") === "metrics"
    );
  });
  await tabs.getByRole("link", { name: "Metrics", exact: true }).click();
  const metricsUrl = new URL((await metricsRequest).url());
  assert.equal(metricsUrl.searchParams.get("space"), "dev");
  assert.equal(metricsUrl.searchParams.get("organization"), "browser-team");
  await page.getByText("Metrics unavailable", { exact: true }).waitFor();
  await page.getByRole("button", { name: "12 hours", exact: true }).click();
  await page
    .locator('#application-metrics[data-selected-range="12h"]')
    .waitFor();
  await tabs.getByRole("link", { name: "Logs", exact: true }).click();
  await page
    .getByRole("heading", { name: "Space logs", exact: true })
    .waitFor();
  await page.getByText("Logs unavailable", { exact: true }).waitFor();
  await page.getByLabel("Search logs", { exact: true }).fill("addon error");
  await page.getByLabel("Log history", { exact: true }).selectOption("30m");
  await page
    .getByRole("checkbox", { name: "Auto-refresh", exact: true })
    .uncheck();
  const logsResponse = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return (
      url.pathname === "/api/ui/space" &&
      url.searchParams.get("search") === "addon error"
    );
  });
  await page
    .getByRole("button", { name: "Apply filters", exact: true })
    .click();
  const filtered = await (await logsResponse).json();
  assert.equal(filtered.data.Range, "30m");
  assert.equal(filtered.data.Follow, false);
  assert.equal(filtered.data.Space, "dev");
  await page.reload();
  await page
    .getByRole("heading", { name: "Space logs", exact: true })
    .waitFor();
  await page.goBack();
  await page
    .getByRole("heading", { name: "Resource usage", exact: true })
    .waitFor();
  await page.goForward();
  await page
    .getByRole("heading", { name: "Space logs", exact: true })
    .waitFor();
  await page.setViewportSize({ width: 390, height: 844 });
  await branch("dev")
    .getByRole("button", { name: "Collapse dev space" })
    .click();
  assert.equal(
    await branch("dev")
      .getByRole("link", { name: "Applications", exact: true })
      .isVisible(),
    false,
  );
  await branch("staging")
    .getByRole("button", { name: "Expand staging space" })
    .click();
  await branch("staging")
    .getByRole("link", { name: "staging", exact: true })
    .click();
  await page.waitForURL((url) => url.searchParams.get("space") === "staging");
  await page
    .getByRole("navigation", { name: "Space sections" })
    .getByRole("link", { name: "Overview", exact: true })
    .waitFor();
  assert.equal(new URL(page.url()).searchParams.get("space"), "staging");
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await sidebar.getByRole("link", { name: "All spaces", exact: true }).click();
  await page.waitForURL((url) => url.searchParams.get("view") === "spaces");
  assert.equal(new URL(page.url()).searchParams.has("space"), false);
  await verifyRowEdges(page.locator(".application-list-item").first());
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(origin + "/?view=infrastructure&tab=clusters");
  await verifyRowEdges(page.locator(".cluster-table tbody tr").first());
}
