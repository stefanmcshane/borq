import assert from "node:assert/strict";

export async function verifyPublicRoutes({ page, origin, api }) {
  await page.goto(
    origin + "/?view=application&organization=browser-team&space=dev",
  );
  const editor = page.locator("#application-editor");
  await editor.locator('[name="name"]').fill("webnginx");
  await editor.locator('[name="image"]').fill("traefik/whoami");
  await editor.locator('[name="port"]').fill("80");
  await editor.getByRole("button", { name: "Add route", exact: true }).click();
  await editor.locator('[name="ingressName"]').fill("tdog");
  await editor.locator('[name="ingressSubdomain"]').fill("web");
  await editor.locator('[name="ingressDomain"]').selectOption("borq.io");
  assert.equal(await editor.locator('[name="ingressHost"]').count(), 0);
  assert.equal(await editor.locator('[name="certificateIssuer"]').count(), 0);
  await editor.locator('[name="ingressSubdomain"]').fill("nested.web");
  assert.equal(
    await editor
      .locator('[name="ingressSubdomain"]')
      .evaluate((input) => input.validity.patternMismatch),
    true,
  );
  await editor.locator('[name="ingressSubdomain"]').fill("web");
  await page.route(
    "**/api/ui/actions",
    (route) => {
      const form = new URLSearchParams(route.request().postData());
      form.set("ingressDomain", "attacker.example");
      return route.continue({ postData: form.toString() });
    },
    { times: 1 },
  );
  await page
    .getByRole("button", { name: "Review changes", exact: true })
    .click();
  await page
    .locator('.form-errors [role="alert"]')
    .filter({ hasText: "Choose a domain allowed" })
    .waitFor();
  assert.equal(
    await editor.locator('[name="ingressSubdomain"]').inputValue(),
    "web",
  );
  assert.equal(
    await editor.locator('[name="image"]').inputValue(),
    "traefik/whoami",
  );
  await editor.locator('[name="ingressDomain"]').selectOption("borq.io");
  await page.locator('[name="editorFormat"][value="yaml"]').click();
  await editor.locator('[name="configuration"]').waitFor();
  assert.match(
    await editor.locator('[name="configuration"]').inputValue(),
    /host: web\.borq\.io/,
  );
  await page.locator('[name="editorFormat"][value="fields"]').click();
  await editor.locator('[name="ingressSubdomain"]').waitFor();
  assert.equal(
    await editor.locator('[name="ingressSubdomain"]').inputValue(),
    "web",
  );
  assert.equal(
    await editor.locator('[name="ingressDomain"]').inputValue(),
    "borq.io",
  );
  const reviewed = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/api/ui/actions",
  );
  await page
    .getByRole("button", { name: "Review changes", exact: true })
    .click();
  const response = await reviewed;
  assert.equal(response.status(), 200, (await response.text()).slice(0, 600));
  await page
    .getByRole("button", { name: "Apply reviewed plan", exact: true })
    .waitFor();
  const plan = await api(
    "/plans/" + new URL(page.url()).searchParams.get("plan"),
  );
  const app = plan.configuration.apps.find((app) => app.name === "webnginx");
  assert.equal(app.image, "traefik/whoami");
  assert.equal(app.port, 80);
  assert.equal(app.ingresses[0].host, "web.borq.io");
  assert.equal(app.ingresses[0].name, "tdog");
  assert.equal(
    (await api("/organizations/browser-team/spaces/dev/apps")).configuration
      .apps.length,
    0,
  );
}
