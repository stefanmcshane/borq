import assert from "node:assert/strict";

// The live cluster test owns resource cleanup; this owns confirmation, errors and navigation.
export async function verifyAppDeletion({ page, origin, telemetryPage }) {
  const workspace = structuredClone(telemetryPage);
  workspace.data.ApplicationTab = "configuration";
  workspace.data.ApplicationSubtab = "container";
  workspace.data.Plan = {};
  workspace.data.Operation = {};
  workspace.data.Location =
    "/?view=application&organization=browser-team&space=dev&app=deletion-preview&tab=configuration";
  const complete = structuredClone(workspace);
  complete.data.View = "applications";
  complete.data.Location =
    "/?view=applications&organization=browser-team&space=dev&operation=delete-web";
  complete.data.Applications = { configuration: { apps: [] }, manifests: [] };
  complete.data.Operation = {
    id: "delete-web",
    phase: "deleting",
    status: "succeeded",
    message: "Deleted web. SQLite volumes and backups were retained.",
  };
  const match = (url) =>
    url.pathname === "/api/ui/page" &&
    url.searchParams.get("app") === "deletion-preview";
  const actions = "**/api/ui/actions";
  let requests = 0;
  await page.route(match, (route) => route.fulfill({ json: workspace }));
  await page.route(actions, (route) => {
    requests++;
    const form = new URLSearchParams(route.request().postData());
    assert.equal(form.get("action"), "delete-app");
    assert.equal(form.get("organization"), "browser-team");
    assert.equal(form.get("space"), "dev");
    assert.equal(form.get("application"), "web");
    return requests === 1
      ? route.fulfill({
          status: 409,
          json: { message: "Wait for the active deployment to finish." },
        })
      : route.fulfill({ json: complete });
  });
  try {
    await page.goto(origin + workspace.data.Location);
    await page.getByText("Delete application", { exact: true }).first().click();
    const remove = page.getByRole("button", {
      name: "Delete application",
      exact: true,
    });
    page.once("dialog", async (dialog) => {
      assert.match(dialog.message(), /Delete web from dev/);
      assert.match(
        dialog.message(),
        /SQLite volumes, backups and secrets will be retained/,
      );
      await dialog.dismiss();
    });
    await remove.click();
    assert.equal(requests, 0);
    page.once("dialog", (dialog) => dialog.accept());
    await remove.click();
    await page
      .getByRole("alert")
      .filter({ hasText: "Wait for the active deployment" })
      .waitFor();
    assert.equal(
      new URL(page.url()).searchParams.get("app"),
      "deletion-preview",
    );
    page.once("dialog", (dialog) => dialog.accept());
    await remove.click();
    await page
      .getByRole("heading", { name: "Application deletion", exact: true })
      .waitFor();
    assert.equal(new URL(page.url()).searchParams.get("view"), "applications");
    assert.equal(new URL(page.url()).searchParams.has("app"), false);
    assert.equal(
      await page.getByRole("button", { name: "Cancel deployment" }).count(),
      0,
    );
    assert.equal(requests, 2);
  } finally {
    await page.unroute(match);
    await page.unroute(actions);
  }
}
