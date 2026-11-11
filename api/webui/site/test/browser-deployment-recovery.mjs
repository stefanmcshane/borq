import assert from "node:assert/strict";

export async function verifyDeploymentRecovery({
  page,
  origin,
  telemetryPage,
}) {
  const workspace = structuredClone(telemetryPage);
  workspace.data.ApplicationTab = "overview";
  workspace.data.Operation = {
    id: "recovery-operation",
    planId: "recovery-plan",
    status: "running",
    message: "UI fixture: observing a deployment.",
  };
  workspace.data.Location =
    "/?view=application&organization=browser-team&space=dev&app=recovery-preview";
  const failed = {
    ...workspace.data.Operation,
    status: "failed",
    message: "UI fixture: the container could not start.",
  };
  const pageMatch = (url) =>
    url.pathname === "/api/ui/page" &&
    url.searchParams.get("app") === "recovery-preview";
  const operationMatch = (url) =>
    url.pathname === "/api/ui/operations/recovery-operation";
  const panelsMatch = (url) =>
    url.pathname === "/api/ui/application" &&
    ["runtime", "deployments"].includes(url.searchParams.get("panel"));
  let operationReads = 0;
  let historyReads = 0;
  await page.route(pageMatch, (route) => route.fulfill({ json: workspace }));
  await page.route(operationMatch, (route) => {
    const url = new URL(route.request().url());
    assert.equal(url.searchParams.get("organization"), "browser-team");
    assert.equal(url.searchParams.get("space"), "dev");
    assert.equal(url.searchParams.get("app"), "web");
    operationReads++;
    if (operationReads === 1)
      return route.fulfill({
        status: 503,
        json: { message: "Deployment status is temporarily unavailable." },
      });
    workspace.data.Operation = failed;
    return route.fulfill({ json: { component: "ui-operation", data: failed } });
  });
  await page.route(panelsMatch, (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("panel") === "runtime") {
      return route.fulfill({
        json: {
          component: "application-runtime",
          data: { Problem: "Live health is unavailable in this UI fixture." },
        },
      });
    }
    historyReads++;
    if (historyReads === 1)
      return route.fulfill({
        status: 503,
        json: { message: "Deployment history is temporarily unavailable." },
      });
    return route.fulfill({
      json: {
        component: "application-deployments",
        data: {
          Organization: "browser-team",
          Space: "dev",
          Application: "web",
          Location: url.pathname + url.search,
          Deployments: [
            {
              operation: failed,
              configuration: workspace.data.Application,
              revision: "fixture-revision",
            },
          ],
        },
      },
    });
  });
  try {
    await page.goto(origin + workspace.data.Location);
    const historyError = page
      .getByRole("alert")
      .filter({ hasText: "Deployment history is temporarily unavailable." });
    await historyError
      .getByRole("button", { name: "Try again", exact: true })
      .click();
    await page
      .getByRole("heading", { name: "Deployment history", exact: true })
      .waitFor();
    assert.equal(historyReads, 2);
    const historyActions = page.locator(".release-actions");
    await historyActions
      .getByRole("link", { name: "View logs", exact: true })
      .waitFor();
    const historyConfiguration = new URL(
      await historyActions
        .getByRole("link", { name: "Review configuration", exact: true })
        .getAttribute("href"),
      origin,
    );
    assert.equal(
      historyConfiguration.searchParams.get("draft"),
      "recovery-plan",
    );
    const statusError = page
      .getByRole("alert")
      .filter({ hasText: "Deployment status is temporarily unavailable." });
    await statusError
      .getByRole("button", { name: "Try again", exact: true })
      .click();
    const operation = page.locator(".operation-panel");
    const logs = operation.getByRole("link", {
      name: "View logs",
      exact: true,
    });
    await logs.waitFor();
    assert.equal(operationReads, 2);
    const logsUrl = new URL(await logs.getAttribute("href"), origin);
    assert.equal(logsUrl.searchParams.get("organization"), "browser-team");
    assert.equal(logsUrl.searchParams.get("space"), "dev");
    assert.equal(logsUrl.searchParams.get("app"), "web");
    assert.equal(logsUrl.searchParams.get("tab"), "observability");
    assert.equal(logsUrl.searchParams.get("subtab"), "logs");
    const configuration = new URL(
      await operation
        .getByRole("link", { name: "Review configuration", exact: true })
        .getAttribute("href"),
      origin,
    );
    assert.equal(configuration.searchParams.get("tab"), "configuration");
  } finally {
    await page.unroute(pageMatch);
    await page.unroute(operationMatch);
    await page.unroute(panelsMatch);
  }
}
