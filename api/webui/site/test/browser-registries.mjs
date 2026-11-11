import assert from "node:assert/strict";

// Browser checks own typed editor selection, scope changes, redaction, and delete
// transport. Real OpenBao/ESO integration owns credential persistence and revocation.
export async function verifyRegistries({ page, origin, api }) {
  await api("/organizations/browser-team/spaces/registry-other", "PUT", {
    cluster: "target",
  });
  await page.goto(origin + "/?view=secret&organization=browser-team&space=dev");
  await page
    .getByLabel("Secret type", { exact: true })
    .selectOption("registry");
  assert.equal(
    await page.getByLabel("Secret name", { exact: true }).inputValue(),
    "",
  );
  await page.getByLabel("Secret name", { exact: true }).fill("github-packages");
  const address = page.getByLabel("Registry address", { exact: true });
  await address.fill("https://ghcr.io/");
  const username = page.getByLabel("Registry username", { exact: true });
  const token = page.getByLabel("Registry password or token", {
    exact: true,
  });
  assert.equal(await token.getAttribute("type"), "password");
  await username.fill("old-space-user");
  await token.fill("old-space-private-token");
  await page.getByLabel("Switch space", { exact: true }).click();
  await page
    .getByRole("navigation", { name: "Breadcrumb" })
    .getByRole("link", { name: "registry-other", exact: true })
    .click();
  await page
    .getByRole("link", { name: "Create a secret", exact: true })
    .click();
  await page
    .getByLabel("Secret type", { exact: true })
    .selectOption("registry");
  assert.equal(
    await page.getByLabel("Secret name", { exact: true }).inputValue(),
    "",
  );
  assert.equal(await address.inputValue(), "");
  assert.equal(await username.inputValue(), "");
  assert.equal(await token.inputValue(), "");

  let submitted;
  let fail = false;
  await page.route("**/api/ui/actions", async (route) => {
    submitted = new URLSearchParams(route.request().postData());
    if (fail) {
      await route.fulfill({
        status: 400,
        json: { message: "Registry credentials could not be saved." },
      });
      return;
    }
    const deleting = submitted.get("action") === "delete-secret";
    const response = await page.request.get(
      origin +
        "/api/ui/page?view=" +
        (deleting ? "secrets" : "secret") +
        "&organization=browser-team&space=registry-other",
    );
    assert.equal(response.status(), 200);
    const workspace = await response.json();
    if (!deleting) {
      workspace.data.Group = {
        name: "github-packages",
        version: 1,
        values: [],
      };
      workspace.data.Secret = {
        name: "github-packages",
        type: "registry",
        version: 1,
        data: { server: "ghcr.io", username: "space-user" },
      };
      workspace.data.GroupLatestVersion = 1;
      workspace.data.GroupTab = "variables";
    }
    workspace.data.Notice = deleting ? "Secret deleted." : "Secret saved.";
    await route.fulfill({ json: workspace });
  });
  try {
    await page
      .getByLabel("Secret name", { exact: true })
      .fill("github-packages");
    await address.fill("https://ghcr.io/");
    await username.fill("space-user");
    await token.fill("space-private-token");
    await page
      .getByRole("button", { name: "Create secret", exact: true })
      .click();
    await page
      .getByRole("button", { name: "Save secret", exact: true })
      .waitFor();
    assert.equal(submitted.get("action"), "put-secret");
    assert.equal(submitted.get("secretType"), "registry");
    assert.equal(submitted.get("name"), "github-packages");
    assert.equal(submitted.get("space"), "registry-other");
    assert.deepEqual(submitted.getAll("variableName"), [
      "server",
      "username",
      "password",
    ]);
    assert.deepEqual(submitted.getAll("variableValue"), [
      "https://ghcr.io/",
      "space-user",
      "space-private-token",
    ]);
    assert.equal(await token.inputValue(), "");
    assert.ok(
      !(await page.locator("body").innerText()).includes("space-private-token"),
    );
    fail = true;
    await token.fill("replacement-private-token");
    await page
      .getByRole("button", { name: "Save secret", exact: true })
      .click();
    await page.getByRole("alert").waitFor();
    assert.match(
      await page.getByRole("alert").innerText(),
      /could not be saved/,
    );
    assert.ok(
      !(await page.locator("body").innerText()).includes(
        "replacement-private-token",
      ),
    );

    // Read the settings section from a safe fixture; no real credential is deleted.
    const response = await page.request.get(
      origin +
        "/api/ui/page?view=secret&organization=browser-team&space=registry-other",
    );
    const settings = await response.json();
    settings.data.Group = { name: "github-packages", version: 1, values: [] };
    settings.data.Secret = {
      name: "github-packages",
      type: "registry",
      version: 1,
      data: {},
    };
    settings.data.GroupLatestVersion = 1;
    settings.data.GroupTab = "settings";
    await page.route("**/api/ui/page?*", (route) =>
      route.fulfill({ json: settings }),
    );
    await page
      .getByRole("navigation", { name: "Secret sections" })
      .getByRole("link", { name: "Settings", exact: true })
      .click();
    await page
      .getByRole("button", { name: "Delete secret", exact: true })
      .waitFor();
    await page.unroute("**/api/ui/page?*");
    fail = false;
    await page
      .getByRole("button", { name: "Delete secret", exact: true })
      .click();
    await page
      .getByRole("heading", { name: "Secrets in registry-other", exact: true })
      .waitFor();
    assert.equal(submitted.get("action"), "delete-secret");
    assert.equal(submitted.get("name"), "github-packages");
    assert.equal(submitted.get("baseVersion"), "1");
  } finally {
    await page.unroute("**/api/ui/actions");
  }

  await page.goto(
    origin + "/?view=infrastructure&node=management&tab=settings",
  );
  const management = page.getByRole("region", { name: "Container registry" });
  assert.equal(
    await management
      .getByLabel("GitHub personal access token", { exact: true })
      .getAttribute("type"),
    "password",
  );
}
