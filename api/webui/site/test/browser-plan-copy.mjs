import assert from "node:assert/strict";

export async function verifyPlanCopy({ page, origin, telemetryPage }) {
  const rawDiff =
    "--- current\n+++ planned\n@@ -1,2 +1,2 @@\n-image: old\n+image: new\n replicas: 1\n";
  const planPage = structuredClone(telemetryPage);
  planPage.data.Plan = {
    id: "preview-plan",
    space: planPage.data.Space,
    configurationDiff: rawDiff,
    manifests: [
      { kind: "Deployment", name: "web", yaml: "apiVersion: apps/v1\n" },
    ],
  };
  planPage.data.Location =
    "/?view=application&organization=browser-team&space=dev&app=plan-preview&plan=preview-plan";
  await page.route(
    (url) =>
      url.pathname === "/api/ui/page" &&
      url.searchParams.get("app") === "plan-preview",
    (route) => route.fulfill({ json: planPage }),
  );
  await page.goto(origin + planPage.data.Location);
  await page.locator("pre.configuration-diff[data-highlighted]").waitFor();
  await page.evaluate(() =>
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: async (text) => {
          window.copiedCode = text;
        },
      },
    }),
  );
  const diffCopy = page
    .locator("pre.configuration-diff")
    .locator("..")
    .getByRole("button", { name: "Copy Configuration changes" });
  await diffCopy.focus();
  await diffCopy.press("Enter");
  assert.equal(await page.evaluate(() => window.copiedCode), rawDiff);
  await page.evaluate(() => {
    navigator.clipboard.writeText = async () => {
      throw Error("disabled");
    };
    document.execCommand = () => false;
  });
  await diffCopy.click();
  const manualCopy = page.locator(".code-copy-manual");
  assert.equal(await manualCopy.inputValue(), rawDiff);
  assert.equal(
    await manualCopy.evaluate((input) =>
      input.value.slice(input.selectionStart, input.selectionEnd),
    ),
    rawDiff,
  );
  await page.route(
    "**/api/ui/actions",
    (route) =>
      route.fulfill({
        status: 409,
        json: { message: "This plan conflicts with the current state." },
      }),
    { times: 1 },
  );
  await page
    .getByRole("button", { name: "Apply reviewed plan", exact: true })
    .click();
  const recovery = page
    .locator('.form-errors [role="alert"]')
    .getByRole("link", { name: "Review configuration", exact: true });
  await recovery.waitFor();
  const destination = new URL(await recovery.getAttribute("href"), origin);
  assert.equal(destination.searchParams.get("organization"), "browser-team");
  assert.equal(destination.searchParams.get("space"), "dev");
  assert.equal(destination.searchParams.get("app"), "web");
  assert.equal(destination.searchParams.get("tab"), "configuration");
  assert.equal(destination.searchParams.get("draft"), "preview-plan");
}

// The manager integration test owns version creation. This check owns the UI's
// ability to submit a reviewed manifest update when app configuration is unchanged.
export async function verifyManifestOnlyPlan({ page, origin, telemetryPage }) {
  const workspace = structuredClone(telemetryPage);
  workspace.data.Plan = {
    id: "manifest-only-plan",
    space: workspace.data.Space,
    configurationDiff: "",
    resourceDiffs: [{ kind: "Deployment", name: "web", action: "update" }],
    manifests: [
      {
        apiVersion: "apps/v1",
        kind: "Deployment",
        metadata: { name: "web" },
        spec: {
          template: {
            spec: { imagePullSecrets: [{ name: "updated-space-credential" }] },
          },
        },
      },
    ],
  };
  workspace.data.Location =
    "/?view=application&organization=browser-team&space=dev&app=manifest-preview&plan=manifest-only-plan";
  const match = (url) =>
    url.pathname === "/api/ui/page" &&
    url.searchParams.get("app") === "manifest-preview";
  await page.route(match, (route) => route.fulfill({ json: workspace }));
  let submitted;
  await page.route(
    "**/api/ui/actions",
    (route) => {
      submitted = new URLSearchParams(route.request().postData());
      return route.fulfill({
        status: 409,
        json: { message: "UI fixture received the reviewed plan." },
      });
    },
    { times: 1 },
  );
  try {
    await page.goto(origin + workspace.data.Location);
    assert.match(
      await page.locator(".configuration-diff").innerText(),
      /No configuration changes/,
    );
    await page
      .getByRole("button", { name: "Apply reviewed plan", exact: true })
      .click();
    await page
      .getByRole("alert")
      .filter({ hasText: "UI fixture received the reviewed plan." })
      .waitFor();
    assert.equal(submitted.get("action"), "apply-plan");
    assert.equal(submitted.get("plan"), "manifest-only-plan");
    assert.equal(submitted.get("organization"), "browser-team");
    assert.equal(submitted.get("space"), "dev");
    assert.equal(submitted.has("configuration"), false);
  } finally {
    await page.unroute(match);
  }
}
