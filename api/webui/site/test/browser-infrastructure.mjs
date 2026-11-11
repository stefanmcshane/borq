import assert from "node:assert/strict";

export async function verifyInfrastructure({
  page,
  origin,
  api,
  kubeconfig,
  createOrganization,
}) {
  await createOrganization("browser-other");
  await api("/organizations/browser-other/clusters/target", "PUT", {
    kubeconfig,
    reconcile: false,
  });
  await api("/organizations/browser-other/spaces/other-space", "PUT", {
    cluster: "target",
  });
  await page.goto(origin + "/?view=infrastructure");
  await page.locator(".component-list a").first().waitFor();
  assert.equal(await page.locator(".application-tabs a").count(), 3);
  assert.equal(await page.locator(".infrastructure-children a").count(), 2);
  assert.equal(
    await page
      .locator("#sidebar-navigation a")
      .filter({ hasText: /^Infrastructure$/ })
      .count(),
    1,
  );
  assert.equal(
    await page
      .locator("#sidebar-navigation a")
      .filter({ hasText: /^Management cluster$/ })
      .count(),
    0,
  );
  const child = (organization) =>
    page.locator(
      `.infrastructure-children a[href*="organization=${organization}"]`,
    );
  await child("browser-team").click();
  await page.waitForURL(
    (url) =>
      url.searchParams.get("node") === "application" &&
      url.searchParams.get("organization") === "browser-team",
  );
  assert.equal(await page.locator(".application-tabs a").count(), 5);
  assert.match(
    await page.locator("#workspace-breadcrumb").innerText(),
    /browser-team \/ target/,
  );
  await page
    .locator('.component-list a[href*="component=cloudnative-pg"]')
    .click();
  await page
    .locator('#component-detail[aria-label="cloudnative-pg configuration"]')
    .waitFor();
  assert.ok(
    await page.locator('.component-list a[href*="component=alloy"]').count(),
  );
  assert.equal(
    await page.locator('.component-list a[href*="component=openbao"]').count(),
    0,
  );
  const effective = page.locator("#component-effective-values");
  const originalYaml = await effective.textContent();
  await page.evaluate(() =>
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: async (value) => {
          window.copiedCode = value;
        },
      },
    }),
  );
  const copy = effective
    .locator("..")
    .getByRole("button", { name: "Copy Values Borq sends to Helm" });
  await copy.focus();
  await copy.press("Enter");
  await page.waitForFunction(() => typeof window.copiedCode === "string");
  assert.equal(await page.evaluate(() => window.copiedCode), originalYaml);
  await page.evaluate(() => {
    navigator.clipboard.writeText = async () => {
      throw Error("disabled");
    };
    document.execCommand = () => {
      window.copiedCode = document.querySelector(".clipboard-fallback").value;
      return true;
    };
  });
  await copy.click();
  assert.equal(await page.evaluate(() => window.copiedCode), originalYaml);
  await page.evaluate(() => {
    document.execCommand = () => false;
  });
  await copy.click();
  assert.match(
    await effective.locator("..").getByRole("status").innerText(),
    /Text selected/,
  );
  await page.locator(".component-customize summary").click();
  await page.locator('[name="values"]').fill("unsaved: keep-me");
  const applyResponse = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === "/api/ui/actions" &&
      response.request().method() === "POST",
  );
  const applyRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === "/api/ui/actions" &&
      request.method() === "POST",
  );
  await page
    .getByRole("button", { name: "Apply cloudnative-pg", exact: true })
    .click();
  const applied = new URLSearchParams((await applyRequest).postData());
  assert.equal(applied.get("view"), "infrastructure");
  assert.equal(applied.get("node"), "application");
  assert.equal(applied.get("organization"), "browser-team");
  assert.equal(applied.get("cluster"), "target");
  assert.equal(applied.get("componentScope"), "postgres");
  assert.equal(applied.get("values"), "unsaved: keep-me");
  const accepted = await applyResponse;
  assert.equal(accepted.status(), 200);
  const componentPage = await accepted.json();
  const operationId = componentPage.data.Settings.Operation.id;
  assert.ok(operationId);
  assert.equal(componentPage.data.Infrastructure.Organization, "browser-team");
  assert.equal(componentPage.data.Infrastructure.Component, "cloudnative-pg");
  assert.equal(
    new URL(componentPage.data.Location, origin).searchParams.has("values"),
    false,
  );
  const savedConfiguration = await api("/management/configuration");
  assert.deepEqual(savedConfiguration.components["cloudnative-pg"].values, {
    unsaved: "keep-me",
  });
  componentPage.data.Settings.Operation = {
    id: operationId,
    status: "queued",
    message: "Queued, no controller result asserted.",
  };

  await page.route(
    (url) =>
      url.pathname === "/api/ui/page" &&
      url.searchParams.get("managementOperation") === operationId,
    (route) => route.fulfill({ json: componentPage }),
  );
  let operationReads = 0;
  await page.route("**/api/ui/management/operation?*", (route) => {
    operationReads++;
    return route.fulfill({
      json: {
        component: "management-operation",
        data: {
          id: operationId,
          status: "queued",
          message: "Waiting " + operationReads,
        },
      },
    });
  });
  await page.goto(origin + componentPage.data.Location);
  await page.locator(".component-customize summary").click();
  const overrideEditor = page.locator('[name="values"]');
  await overrideEditor.fill("unsaved: newer-draft");
  await overrideEditor.evaluate((node) => {
    window.unsavedEditor = node;
  });
  await page.waitForFunction(() =>
    document
      .querySelector(".infrastructure-operation")
      ?.textContent.includes("Waiting"),
  );
  assert.equal(await overrideEditor.inputValue(), "unsaved: newer-draft");
  assert.ok(
    await overrideEditor.evaluate((node) => node === window.unsavedEditor),
  );
  const withoutOperation = new URL(componentPage.data.Location, origin);
  withoutOperation.searchParams.delete("managementOperation");
  await page.goto(withoutOperation.href);
  await page
    .locator('#component-detail[aria-label="cloudnative-pg configuration"]')
    .waitFor();
  await page.unrouteAll({ behavior: "wait" });
  await page
    .getByRole("navigation", { name: "Cluster sections" })
    .locator('a[href*="tab=spaces"]')
    .click();
  await page.getByRole("heading", { name: "Spaces on target" }).waitFor();
  assert.match(await page.locator("#workspace").innerText(), /dev/);
  assert.doesNotMatch(
    await page.locator("#workspace").innerText(),
    /other-space/,
  );
  await page
    .getByRole("navigation", { name: "Cluster sections" })
    .getByRole("link", { name: "Settings", exact: true })
    .click();
  await page.getByRole("heading", { name: "Cluster connection" }).waitFor();
  assert.match(
    await page
      .getByRole("link", { name: "Download kubeconfig" })
      .getAttribute("href"),
    /organization=browser-team/,
  );
  await child("browser-other").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#workspace-breadcrumb")
      ?.textContent.includes("browser-other / target"),
  );
  await page.goBack();
  await page.getByRole("heading", { name: "Cluster connection" }).waitFor();
  assert.match(
    await page.locator("#workspace-breadcrumb").innerText(),
    /browser-team \/ target/,
  );
  await page.goForward();
  await page.waitForFunction(() =>
    document
      .querySelector("#workspace-breadcrumb")
      ?.textContent.includes("browser-other / target"),
  );
  await page
    .locator("#sidebar-navigation a")
    .filter({ hasText: /^Infrastructure$/ })
    .click();
  await page.waitForURL(
    (url) =>
      url.searchParams.get("node") === "management" &&
      url.searchParams.get("tab") === "components",
  );
  await page
    .getByRole("navigation", { name: "Cluster sections" })
    .getByRole("link", { name: "Settings", exact: true })
    .click();
  await page.getByRole("heading", { name: "Shared settings" }).waitFor();
  await page
    .getByRole("heading", { name: "Offsite copy of S3 data", exact: true })
    .waitFor();
  const coverage = page.getByRole("region", {
    name: "Backup coverage",
    exact: true,
  });
  assert.equal(await coverage.getByRole("table").count(), 0);
  await coverage
    .getByRole("link", { name: "Check backup coverage", exact: true })
    .click();
  await coverage.getByRole("table").waitFor();
  assert.match(
    await coverage.getByRole("status").innerText(),
    /Some backups need attention/,
  );
  assert.match(
    await coverage
      .getByRole("row")
      .filter({ hasText: /management.*sqlite/ })
      .innerText(),
    /disabled.*None observed/,
  );
  const offsite = page.locator("[data-offsite-settings]");
  assert.equal(await offsite.getByLabel("Copy interval").inputValue(), "6h");
  assert.equal(
    await offsite.getByLabel("Offsite snapshots to retain").inputValue(),
    "14",
  );
  assert.ok(
    await page
      .getByRole("button", { name: "Disable offsite backups", exact: true })
      .isDisabled(),
  );
  assert.equal(
    await page
      .getByRole("button", { name: "Remove unused archive connection" })
      .count(),
    0,
  );
  assert.equal(await page.locator('input[name="credentialsRef"]').count(), 0);
  assert.ok(
    await offsite
      .getByLabel("Backup encryption password", { exact: true })
      .isVisible(),
  );
  await offsite.getByLabel("Enable offsite backups").check();
  await offsite
    .getByLabel("Offsite S3 destination", { exact: true })
    .fill("https://offsite.example/backups/borq");
  assert.equal(
    await offsite
      .getByLabel("Offsite region", { exact: true })
      .evaluate((input) => input.validity.valueMissing),
    true,
  );
  await offsite.getByLabel("Offsite region", { exact: true }).fill("us-east-1");
  await offsite
    .getByLabel("Backup encryption password", { exact: true })
    .fill("never-echo-browser-password");
  assert.equal(
    await offsite
      .getByLabel("Offsite access key", { exact: true })
      .evaluate((input) => input.validity.valueMissing),
    true,
  );
  assert.equal(await offsite.evaluate((form) => form.checkValidity()), false);
  const settingsPath = "/?view=infrastructure&node=management&tab=settings";
  const saved = await (
    await page.request.get(origin + "/api/ui/page" + settingsPath.slice(1))
  ).json();
  saved.data.Settings.Offsite = {
    enabled: true,
    repository: "s3:https://offsite.example/backups/borq",
    region: "us-east-1",
    interval: "6h",
    keepLast: 14,
    credentialsRef: "saved-offsite-reference",
  };
  const initialSettings = (url) =>
    url.pathname === "/api/ui/page" &&
    url.searchParams.get("tab") === "settings" &&
    !url.searchParams.has("managementOperation");
  await page.route(initialSettings, (route) => route.fulfill({ json: saved }), {
    times: 1,
  });
  await page.goto(origin + settingsPath);
  assert.equal(
    await page
      .getByLabel("Offsite S3 destination", { exact: true })
      .inputValue(),
    "https://offsite.example/backups/borq",
  );
  async function verifyStorageSave({
    action,
    operationId,
    form,
    field,
    draft,
    status,
    message,
    update,
  }) {
    const before = await form.getByLabel(field, { exact: true }).inputValue();
    await form.getByLabel(field, { exact: true }).fill(draft);
    const waiting = structuredClone(saved);
    waiting.data.Settings.Operation = {
      id: operationId,
      status: "running",
      message: "Testing the S3 connection before saving settings.",
    };
    waiting.data.Location =
      settingsPath + "&managementOperation=" + operationId;
    const finished = structuredClone(waiting);
    finished.data.Settings.Operation = { id: operationId, status, message };
    if (update) update(finished.data.Settings);
    const refresh = (url) =>
      url.pathname === "/api/ui/page" &&
      url.searchParams.get("managementOperation") === operationId;
    const poll = (url) =>
      url.pathname === "/api/ui/management/operation" &&
      url.searchParams.get("managementOperation") === operationId;
    let finishTest;
    const connectionTest = new Promise((resolve) => {
      finishTest = resolve;
    });
    await page.route(
      "**/api/ui/actions",
      async (route) => {
        assert.equal(
          new URLSearchParams(route.request().postData()).get("action"),
          action,
        );
        await route.fulfill({ json: waiting });
      },
      { times: 1 },
    );
    await page.route(poll, async (route) => {
      await connectionTest;
      await route.fulfill({
        json: {
          component: "management-operation",
          data: finished.data.Settings.Operation,
        },
      });
    });
    await page.route(refresh, (route) => route.fulfill({ json: finished }));
    const polling = page.waitForRequest((request) =>
      poll(new URL(request.url())),
    );
    const reloaded = page.waitForResponse((response) =>
      refresh(new URL(response.url())),
    );
    const settled = Promise.allSettled([polling, reloaded]);
    try {
      await form
        .getByRole("button", { name: "Test and save", exact: true })
        .click();
      await page
        .getByText("Testing the S3 connection before saving settings.", {
          exact: false,
        })
        .waitFor();
      assert.equal(
        await form.getByLabel(field, { exact: true }).inputValue(),
        before,
      );
      const saves = page.getByRole("button", {
        name: "Test and save",
        exact: true,
        includeHidden: true,
      });
      assert.equal(await saves.count(), 1);
      assert.ok(await saves.nth(0).isDisabled());
      assert.ok(
        await page
          .getByRole("button", { name: "Disable offsite backups", exact: true })
          .isDisabled(),
      );
      assert.doesNotMatch(
        await page.locator("#notice").innerText(),
        /validated and saved/,
      );
      await polling;
      finishTest();
      await reloaded;
      await page.getByText(message, { exact: false }).waitFor();
      assert.equal(
        await form.getByLabel(field, { exact: true }).inputValue(),
        status === "succeeded" ? draft : before,
      );
      assert.ok(
        await form
          .getByRole("button", {
            name: "Test and save",
            exact: true,
            includeHidden: true,
          })
          .isEnabled(),
      );
      if (status === "failed") {
        assert.match(
          await page.getByRole("alert").innerText(),
          /saved settings were kept/,
        );
      }
    } finally {
      finishTest();
      await settled;
      await page.unroute(poll);
      await page.unroute(refresh);
    }
  }
  await verifyStorageSave({
    action: "put-management-offsite",
    operationId: "browser-offsite-failure",
    form: page.locator("[data-offsite-settings]"),
    field: "Offsite region",
    draft: "unreachable-region",
    status: "failed",
    message: "S3 connection test failed. Your saved settings were kept.",
  });
  await verifyStorageSave({
    action: "put-management-offsite",
    operationId: "browser-offsite-test",
    form: page.locator("[data-offsite-settings]"),
    field: "Offsite region",
    draft: "eu-west-1",
    status: "succeeded",
    message: "S3 connection test succeeded and offsite settings were saved.",
    update: (settings) => {
      settings.Offsite.region = "eu-west-1";
    },
  });
  saved.data.Settings.Offsite.region = "eu-west-1";
  const disable = page.getByRole("button", {
    name: "Disable offsite backups",
    exact: true,
  });
  assert.ok(await disable.isEnabled());
  const pendingOffsite = structuredClone(saved);
  pendingOffsite.data.Settings.Operation = {
    id: "browser-offsite-change",
    status: "running",
  };
  pendingOffsite.data.Location =
    settingsPath + "&managementOperation=browser-offsite-change";
  const completedOffsite = structuredClone(pendingOffsite);
  completedOffsite.data.Settings.Operation.status = "succeeded";
  completedOffsite.data.Settings.Offsite.enabled = false;
  const refreshedSettings = (url) =>
    url.pathname === "/api/ui/page" &&
    url.searchParams.get("managementOperation") === "browser-offsite-change";
  const operationPoll = (url) =>
    url.pathname === "/api/ui/management/operation" &&
    url.searchParams.get("managementOperation") === "browser-offsite-change";
  await page.route(
    "**/api/ui/actions",
    async (route) => {
      assert.equal(
        new URLSearchParams(route.request().postData()).get("action"),
        "disable-management-offsite",
      );
      await route.fulfill({ json: pendingOffsite });
    },
    { times: 1 },
  );
  await page.route(operationPoll, (route) =>
    route.fulfill({
      json: {
        component: "management-operation",
        data: { id: "browser-offsite-change", status: "succeeded" },
      },
    }),
  );
  await page.route(refreshedSettings, (route) =>
    route.fulfill({ json: completedOffsite }),
  );
  const settingsReload = page.waitForResponse((response) =>
    refreshedSettings(new URL(response.url())),
  );
  await disable.click();
  await settingsReload;
  await page.waitForFunction(
    () =>
      !document.querySelector('[data-offsite-settings] [name="enabled"]')
        .checked,
  );
  assert.ok(await disable.isDisabled());
  assert.equal(await page.locator('input[name="credentialsRef"]').count(), 0);
  assert.equal(
    (await page.locator("#workspace").innerText()).includes(
      "saved-offsite-reference",
    ),
    false,
  );
  const savedFields = await page
    .locator("[data-offsite-settings]")
    .evaluate((form) => Array.from(new FormData(form).keys()));
  assert.equal(savedFields.includes("credentialsRef"), false);
  await page.getByText("Change offsite credentials", { exact: true }).click();
  assert.equal(
    await page
      .getByLabel("Backup encryption password", { exact: true })
      .inputValue(),
    "",
  );
  assert.equal(
    await page
      .locator("[data-offsite-settings]")
      .evaluate((form) => form.checkValidity()),
    true,
  );
  await page.unroute(operationPoll);
  await page.unroute(refreshedSettings);
  assert.equal(
    await page.locator('[name="action"][value="grant-managed-addons"]').count(),
    0,
  );
  assert.equal(await page.locator("#component-catalog").count(), 0);
  await page.setViewportSize({ width: 390, height: 844 });
  await page
    .locator("#sidebar-navigation a")
    .filter({ hasText: /^Infrastructure$/ })
    .click();
  await page.waitForURL(
    (url) =>
      url.searchParams.get("node") === "management" &&
      url.searchParams.get("tab") === "components",
  );
  await page.getByLabel("Component", { exact: true }).waitFor();
  await page
    .getByLabel("Component", { exact: true })
    .selectOption(
      await page
        .getByLabel("Component", { exact: true })
        .locator("option")
        .filter({ hasText: "rustfs" })
        .getAttribute("value"),
    );
  await page
    .locator('#component-detail[aria-label="rustfs configuration"]')
    .waitFor();
  await page.locator("[data-theme-toggle]").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  const lightBackground = await page.evaluate(
    () => getComputedStyle(document.body).backgroundColor,
  );
  await page.locator("[data-theme-toggle]").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  assert.notEqual(
    await page.evaluate(() => getComputedStyle(document.body).backgroundColor),
    lightBackground,
  );
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.setViewportSize({ width: 1280, height: 900 });
}
