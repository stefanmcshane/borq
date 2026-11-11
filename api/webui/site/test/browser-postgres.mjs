import assert from "node:assert/strict";
import { parse as parseYaml } from "yaml";

export async function verifyPostgres({ page, origin }) {
  await page.goto(
    origin + "/?view=postgres&organization=browser-team&space=dev",
  );
  const createButtons = page.getByRole("button", {
    name: "Create database",
    exact: true,
  });
  const envelope = await (
    await page.request.get(
      origin + "/api/ui/page?view=postgres&organization=browser-team&space=dev",
    )
  ).json();
  assert.equal(
    envelope.data.Organizations.find((org) => org.name === "browser-team")
      .managedAddons,
    true,
  );
  assert.equal(
    await page
      .getByRole("button", { name: "Enable managed PostgreSQL", exact: true })
      .count(),
    0,
  );
  assert.ok(await createButtons.first().isEnabled());
  await page
    .getByRole("button", { name: "Create database", exact: true })
    .first()
    .click();
  const databaseName = page.locator("#postgres-create [data-postgres-name]");
  await page.waitForFunction(
    () =>
      document.querySelector("#postgres-create [data-postgres-name]")?.value
        .length > 0,
  );
  const suggestedName = await databaseName.inputValue();
  assert.match(suggestedName, /^[a-z]+-[a-z]+$/);
  assert.ok(suggestedName.length <= 10);
  await databaseName.fill("Bad_Name");
  assert.equal(await databaseName.inputValue(), "bad_name");
  assert.equal(
    await databaseName.evaluate((input) => input.validity.patternMismatch),
    true,
  );
  await databaseName.fill("database-one");
  assert.equal(
    await databaseName.evaluate((input) => input.validity.patternMismatch),
    false,
  );
  const createForm = page.locator("#postgres-create form");
  assert.equal(await databaseName.getAttribute("maxlength"), "47");
  assert.equal(await createForm.locator("details").getAttribute("open"), null);
  await page.locator("#postgres-create [data-dialog-close]").click();
  await page
    .getByRole("button", { name: "Create database", exact: true })
    .first()
    .click();
  assert.equal(await databaseName.inputValue(), "database-one");
  await createForm.locator("summary").click();
  const source = createForm.locator("[data-postgres-source]");
  const yamlTab = createForm.getByRole("tab", {
    name: "YAML",
    exact: true,
  });
  const settingsTab = createForm.getByRole("tab", {
    name: "Settings",
    exact: true,
  });
  await settingsTab.focus();
  await settingsTab.press("End");
  assert.equal(await yamlTab.getAttribute("aria-selected"), "true");
  await source.fill(
    "# Keep comments\ninstances: 4\nstorage: 6Gi\nimage: postgres:17.6\nbackup:\n  enabled: true\n  schedule: 0 1 * * *\n  restoreCheck:\n    schedule: 0 5 * * 3\n",
  );
  await settingsTab.click();
  const instances = createForm.locator('[data-postgres-setting="instances"]');
  const storage = createForm.locator('[data-postgres-setting="storage"]');
  const backups = createForm.locator('[data-postgres-setting="backups"]');
  const restoreSchedule = createForm.locator(
    '[data-postgres-setting="restoreSchedule"]',
  );
  assert.equal(await instances.inputValue(), "4");
  await instances.fill("");
  await storage.fill("");
  let specification = parseYaml(
    await createForm.locator('[name="configuration"]').inputValue(),
  );
  assert.equal(specification.instances, undefined);
  assert.equal(specification.storage, undefined);
  await instances.fill("2");
  await storage.fill("8Gi");
  await restoreSchedule.fill("0 6 * * 1");
  await backups.uncheck();
  assert.ok(await restoreSchedule.isDisabled());
  specification = parseYaml(
    await createForm.locator('[name="configuration"]').inputValue(),
  );
  assert.deepEqual(specification, {
    instances: 2,
    storage: "8Gi",
    image: "postgres:17.6",
    backup: {
      enabled: false,
      schedule: "0 1 * * *",
      restoreCheck: { schedule: "0 6 * * 1" },
    },
  });
  assert.match(await source.inputValue(), /# Keep comments/);
  await yamlTab.click();
  for (const invalid of [
    "instances: [",
    "instances: many",
    "backup: false",
    "instances: 1\ninstances: 2",
    "---\n{}\n---\n{}",
    "image: &image postgres:17\ndatabase: *image",
    "defaults: &defaults {}\n<<: *defaults",
  ]) {
    await source.fill(invalid);
    assert.ok(await settingsTab.isDisabled());
    assert.equal(await source.inputValue(), invalid);
    assert.equal(
      await createForm.evaluate((form) => form.checkValidity()),
      false,
    );
    assert.ok(
      (await createForm.locator("[data-postgres-error]").innerText()).length >
        0,
    );
  }
  await source.fill(
    "backup:\n  enabled: true\n  restoreCheck:\n    enabled: false\n    schedule: ''\n",
  );
  await settingsTab.click();
  assert.equal(
    await restoreSchedule.isDisabled(),
    false,
    "An unsupported legacy flag cannot disable mandatory restore tests",
  );
  assert.equal(
    await createForm.evaluate((form) => form.checkValidity()),
    false,
  );
  await yamlTab.click();
  await source.fill(
    "backup:\n  enabled: true\n  restoreCheck:\n    schedule: ''\n",
  );
  await settingsTab.click();
  assert.equal(await restoreSchedule.isDisabled(), false);
  assert.equal(
    await createForm.evaluate((form) => form.checkValidity()),
    false,
  );
  await yamlTab.click();
  await source.fill("# Keep defaults\n");
  assert.equal(await source.inputValue(), "# Keep defaults\n");
  assert.deepEqual(
    parseYaml(await createForm.locator('[name="configuration"]').inputValue()),
    {},
  );
  const posted = createForm.evaluate((form) => Array.from(new FormData(form)));
  assert.ok(
    !(await posted).some(([key]) =>
      ["instances", "storage", "restoreSchedule"].includes(key),
    ),
  );
  await page.locator("#postgres-create [data-dialog-close]").click();
  const postgresEnvelope = await (
    await page.request.get(
      origin + "/api/ui/page?view=postgres&organization=browser-team&space=dev",
    )
  ).json();
  postgresEnvelope.data.View = "postgres-detail";
  postgresEnvelope.data.SectionView = "postgres";
  postgresEnvelope.data.Postgres.Tab = "configuration";
  postgresEnvelope.data.Postgres.Database = {
    name: "saved-db",
    specification: {
      instances: 3,
      storage: "4Gi",
      credentials: { rotationPeriod: "24h" },
      backup: {
        enabled: false,
        schedule: "0 1 * * *",
        restoreCheck: { schedule: "0 4 * * 2" },
      },
    },
  };
  postgresEnvelope.data.Location =
    "/?view=postgres-detail&organization=browser-team&space=dev&postgres=saved-db&tab=configuration";
  await page.route(
    (url) =>
      url.pathname === "/api/ui/page" &&
      url.searchParams.get("postgres") === "saved-db",
    (route) => route.fulfill({ json: postgresEnvelope }),
  );
  await page.goto(origin + postgresEnvelope.data.Location);
  await page.locator('[data-postgres-setting="backups"]').waitFor();
  assert.equal(
    await page.locator('[data-postgres-setting="backups"]').isChecked(),
    false,
  );
  assert.equal(
    await page.locator('[data-postgres-setting="instances"]').inputValue(),
    "3",
  );
  assert.deepEqual(
    parseYaml(await page.locator('[name="configuration"]').inputValue()),
    postgresEnvelope.data.Postgres.Database.specification,
  );
  await page.unrouteAll({ behavior: "wait" });
}
