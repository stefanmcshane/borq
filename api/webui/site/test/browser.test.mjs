import assert from "node:assert/strict";
import { test } from "node:test";
import { access, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import { chromium } from "playwright-core";
import { verifyPostgres } from "./browser-postgres.mjs";
import { verifyInfrastructure } from "./browser-infrastructure.mjs";
import {
  verifyPlanCopy,
  verifyManifestOnlyPlan,
} from "./browser-plan-copy.mjs";
import { verifyRegistries } from "./browser-registries.mjs";
import { verifySpaces } from "./browser-spaces.mjs";
import { verifyDeploymentRecovery } from "./browser-deployment-recovery.mjs";
import { verifyAppDeletion } from "./browser-app-deletion.mjs";
import {
  verifyOrganizationOidc,
  verifyOrganizationSignIn,
} from "./browser-oidc.mjs";
import { serveApplicationCluster } from "./browser-application-cluster.mjs";
import { verifyPublicRoutes } from "./browser-public-routes.mjs";
import { startIdentityFixture } from "./browser-identity-fixture.mjs";
import { verifyOrganizationMembers } from "./browser-members.mjs";

test(
  "embedded Astro site preserves navigation and draft workflows",
  { timeout: 110000 },
  async () => {
    const binary = resolve(
      process.env.BORQ_UI_TEST_BINARY || "../../../dist/borq",
    );
    await access(binary);
    const directory = await mkdtemp(join(tmpdir(), "borq-astro-browser-"));
    const identity = await startIdentityFixture(directory);
    const cluster = createServer((request, response) => {
      response.setHeader("Content-Type", "application/json");
      if (serveApplicationCluster(request, response)) return;
      response.statusCode = 503;
      response.end(
        JSON.stringify({
          message:
            "Browser fixture only supports read-only discovery and absent resources.",
        }),
      );
    });
    cluster.listen(0, "127.0.0.1");
    await once(cluster, "listening");
    const clusterAddress = cluster.address();
    assert.ok(clusterAddress && typeof clusterAddress !== "string");
    const portListener = createServer();
    portListener.listen(0, "127.0.0.1");
    await once(portListener, "listening");
    const address = portListener.address();
    assert.ok(address && typeof address !== "string");
    const port = address.port;
    await new Promise((resolve) => portListener.close(resolve));
    const origin = `http://127.0.0.1:${port}`;
    const server = spawn(
      binary,
      [
        "server",
        `--port=${port}`,
        `--data-dir=${directory}`,
        "--infrastructure-enabled=false",
        "--management-enabled=false",
        `--management-kubeconfig=${identity.kubeconfigPath}`,
        "--management-openbao-url=http://127.0.0.1:18200",
        "--management-telemetry-url=http://127.0.0.1:17834",
      ],
      { cwd: directory, stdio: ["ignore", "pipe", "pipe"] },
    );
    let output = "";
    for (const pipe of [server.stdout, server.stderr])
      pipe.on("data", (data) => {
        output = (output + data.toString()).slice(-12000);
      });
    const exited = once(server, "exit");
    let browser;
    let page;
    try {
      for (let attempt = 0; attempt < 100; attempt++) {
        try {
          const response = await fetch(origin + "/api/healthz");
          if (response.ok) break;
        } catch {}
        if (server.exitCode !== null)
          throw new Error("Borq stopped during startup: " + output);
        if (attempt === 99)
          throw new Error("Borq did not become ready: " + output);
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
      browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH,
      });
      page = await browser.newPage();
      page.setDefaultTimeout(10000);
      const failures = [];
      page.on("pageerror", (error) => failures.push(error.message));
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (url.protocol.startsWith("http"))
          assert.equal(
            url.origin,
            origin,
            "All production assets and data must come from Borq.",
          );
      });
      const adminToken = (
        await readFile(join(directory, "admin-token"), "utf8")
      ).trim();
      await page.goto(origin + "/login/admin");
      await page.getByLabel("Admin token", { exact: true }).fill(adminToken);
      await page
        .getByRole("button", { name: "Continue as administrator", exact: true })
        .click();
      await page.waitForURL(origin + "/");
      await page
        .getByRole("heading", { name: "Create your first organization" })
        .waitFor();
      await page.getByLabel("Switch organization").click();
      await page
        .getByRole("button", { name: "Add organization", exact: true })
        .click();
      await page
        .getByRole("dialog")
        .getByLabel("Organization name")
        .fill(" browser-team ");
      assert.equal(
        await page.getByLabel("Sign-in provider", { exact: true }).inputValue(),
        "none",
      );
      await page
        .getByRole("dialog")
        .getByLabel("Organization name")
        .press("Enter");
      await page
        .getByRole("heading", { name: "Link a cluster", exact: true })
        .waitFor();
      await page
        .getByRole("dialog")
        .getByRole("button", { name: "Close dialog" })
        .click();
      assert.equal(
        new URL(page.url()).searchParams.get("organization"),
        "browser-team",
      );
      const api = async (path, method = "GET", body) => {
        const response = await fetch(origin + "/api/v1" + path, {
          method,
          headers: {
            "Content-Type": "application/json",
            Authorization: "Bearer " + adminToken,
          },
          body: body === undefined ? undefined : JSON.stringify(body),
        });
        const data = await response.json();
        assert.ok(
          response.ok,
          `API ${method} ${path} returned ${response.status}: ${JSON.stringify(data).slice(0, 800)}`,
        );
        return data;
      };
      await page
        .getByRole("link", { name: "Authentication", exact: true })
        .click();
      const initialOidc = page.getByRole("form", {
        name: "OIDC provider settings",
        exact: true,
      });
      await initialOidc
        .getByLabel("Administrator email", { exact: true })
        .fill("browser-admin@example.test");
      await initialOidc
        .getByLabel("Issuer URL", { exact: true })
        .fill(identity.issuer);
      await initialOidc
        .getByLabel("Client ID", { exact: true })
        .fill("browser-team-client");
      await initialOidc
        .getByLabel("Client secret", { exact: true })
        .fill("browser-team-client-secret");
      await initialOidc
        .getByLabel("Borq public URL", { exact: true })
        .fill(origin);
      await initialOidc
        .getByRole("button", { name: "Save and check discovery", exact: true })
        .click();
      await page
        .getByText("Provider settings saved and discovery checked.", {
          exact: false,
        })
        .waitFor();
      const createOrganization = (name) =>
        api(`/organizations/${name}`, "PUT", {
          adminEmail: "browser-admin@example.test",
          oidc: {
            issuerUrl: identity.issuer,
            clientId: name + "-client",
            clientSecret: name + "-private-secret",
            borqPublicUrl: origin,
            revision: 0,
          },
        });
      await verifyOrganizationSignIn({ page, origin, issuer: identity.issuer });
      await verifyOrganizationOidc({ page, origin, api, createOrganization });
      await verifyOrganizationMembers({
        page,
        origin,
        api,
        issuer: identity.issuer,
      });
      const kubeconfig = `apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: http://127.0.0.1:${clusterAddress.port}\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n`;
      await api("/organizations/browser-team/clusters/target", "PUT", {
        kubeconfig,
        reconcile: false,
      });
      await api("/organizations/browser-team/spaces/dev", "PUT", {
        cluster: "target",
      });
      await verifyPublicRoutes({ page, origin, api });
      const pasteInput = (field, text) =>
        field.evaluate((input, text) => {
          const clipboard = new DataTransfer();
          clipboard.setData("text/plain", text);
          input.dispatchEvent(
            new ClipboardEvent("paste", {
              clipboardData: clipboard,
              bubbles: true,
              cancelable: true,
            }),
          );
        }, text);
      await page.goto(
        origin + "/?view=infrastructure&node=management&tab=settings",
      );
      const offsiteDestination = page.getByLabel("Offsite S3 destination", {
        exact: true,
      });
      await offsiteDestination.fill("https://s3.example.com/old");
      await offsiteDestination.press("End");
      for (let index = 0; index < 3; index++)
        await offsiteDestination.press("Shift+ArrowLeft");
      await pasteInput(offsiteDestination, " new ");
      assert.equal(
        await offsiteDestination.inputValue(),
        "https://s3.example.com/new",
      );
      const appPageRoute =
        "**/api/ui/page?view=application&organization=browser-team&space=dev";
      await page.route(appPageRoute, async (route) => {
        const response = await route.fetch();
        const result = await response.json();
        result.data.ApplicationEditor.GroupNames = ["shared-config"];
        await route.fulfill({ response, json: result });
      });
      await page.goto(
        origin + "/?view=application&organization=browser-team&space=dev",
      );
      await page.locator('#application-editor [name="name"]').fill(" web ");
      await page
        .locator('#application-editor [name="image"]')
        .fill(" nginx:1.27.5 ");
      const applicationPort = page.locator('#application-editor [name="port"]');
      await applicationPort.fill("1234");
      await applicationPort.press("End");
      await applicationPort.press("Shift+ArrowLeft");
      await applicationPort.press("Shift+ArrowLeft");
      await pasteInput(applicationPort, " 80 ");
      assert.equal(await applicationPort.inputValue(), "1280");
      await applicationPort.press("ControlOrMeta+A");
      await pasteInput(applicationPort, " 8080 ");
      assert.equal(await applicationPort.inputValue(), "8080");
      await page
        .getByRole("checkbox", { name: "Enable SQLite", exact: true })
        .check();
      // Remote HTTP installations have getRandomValues but no randomUUID.
      await page.evaluate(() =>
        Object.defineProperty(crypto, "randomUUID", { value: undefined }),
      );
      const appSecrets = page.locator("#application-editor");
      await page.unroute(appPageRoute);
      assert.equal(
        await appSecrets.locator('[name="environment"]').isVisible(),
        true,
      );
      assert.equal(
        await appSecrets.locator('[name="environment"]').isEnabled(),
        true,
      );
      await appSecrets
        .locator('[name="environment"]')
        .selectOption("shared-config");
      await appSecrets.locator('[name="environmentVersion"]').fill("4");
      await appSecrets
        .getByText("Add custom secrets (optional)", { exact: true })
        .click();
      await appSecrets
        .getByRole("button", { name: "+ Add secret", exact: true })
        .click();
      await appSecrets.getByLabel("Key", { exact: true }).fill(" TOKEN ");
      const appSecretValue = " <private-app-token>\nsecond line\n";
      await appSecrets
        .getByLabel("Value", { exact: true })
        .fill(appSecretValue);
      await appSecrets
        .getByRole("button", { name: "+ Add secret", exact: true })
        .click();
      await appSecrets
        .getByRole("group", { name: "App secret", exact: true })
        .last()
        .getByRole("button", { name: "Remove secret", exact: true })
        .click();
      assert.equal(
        await appSecrets
          .getByRole("group", { name: "App secret", exact: true })
          .count(),
        1,
      );
      assert.equal(
        await appSecrets.locator('[name="environment"]').inputValue(),
        "shared-config",
      );
      assert.equal(
        await appSecrets.locator('[name="environment"]').isEnabled(),
        true,
      );
      await page.locator('[name="editorFormat"][value="yaml"]').click();
      await page
        .locator('#application-editor [name="configuration"]')
        .waitFor();
      assert.match(
        await page.locator('[name="configuration"]').inputValue(),
        /name: web/,
      );
      assert.doesNotMatch(
        await appSecrets.locator('[name="configuration"]').inputValue(),
        /private-app-token/,
      );
      assert.equal(
        await appSecrets.getByLabel("Value", { exact: true }).inputValue(),
        appSecretValue,
      );
      await page.locator('[name="editorFormat"][value="fields"]').click();
      await page.locator('#application-editor [name="image"]').waitFor();
      assert.equal(
        await page.locator('#application-editor [name="image"]').inputValue(),
        "nginx:1.27.5",
      );
      const sqlite = page.getByRole("checkbox", {
        name: "Enable SQLite",
        exact: true,
      });
      assert.equal(await sqlite.isChecked(), true);
      await sqlite.uncheck();
      assert.equal(
        await appSecrets.getByLabel("Key", { exact: true }).inputValue(),
        "TOKEN",
      );
      assert.equal(
        await appSecrets.getByLabel("Value", { exact: true }).inputValue(),
        appSecretValue,
      );
      await appSecrets.getByLabel("Key", { exact: true }).fill(" TOKEN ");
      // This cluster fixture has no secret store; API tests own publication.
      await page.route("**/api/ui/actions", (route) =>
        route.fulfill({
          status: 503,
          json: { message: "Secrets are temporarily unavailable." },
        }),
      );
      const submittedSecrets = page.waitForRequest(
        (request) => new URL(request.url()).pathname === "/api/ui/actions",
      );
      await page
        .getByRole("button", { name: "Review changes", exact: true })
        .evaluate((button) => button.form.requestSubmit(button));
      await page.locator('.form-errors [role="alert"]').waitFor();
      const submittedSecretForm = new URLSearchParams(
        (await submittedSecrets).postData(),
      );
      const secretDraft = submittedSecretForm.get("secretDraft");
      assert.ok(secretDraft);
      assert.equal(submittedSecretForm.get("environment"), "shared-config");
      assert.equal(submittedSecretForm.get("environmentVersion"), "4");
      assert.equal(submittedSecretForm.get("variableName"), "TOKEN");
      assert.equal(
        submittedSecretForm.get("variableValue").replaceAll("\r\n", "\n"),
        appSecretValue,
      );
      assert.equal(
        await appSecrets.getByLabel("Value", { exact: true }).inputValue(),
        appSecretValue,
      );
      assert.equal(
        await appSecrets.locator('[name="secretDraft"]').inputValue(),
        secretDraft,
      );
      await appSecrets
        .getByLabel("Value", { exact: true })
        .fill("updated-token");
      assert.notEqual(
        await appSecrets.locator('[name="secretDraft"]').inputValue(),
        secretDraft,
      );
      await page.unroute("**/api/ui/actions");
      const configurationError = page.locator('.form-errors [role="alert"]');
      await configurationError
        .getByRole("button", { name: "Review configuration", exact: true })
        .click();
      assert.ok(
        await page.evaluate(() =>
          document
            .querySelector("#application-editor")
            .contains(document.activeElement),
        ),
      );
      assert.equal(
        await page.locator('#application-editor [name="image"]').inputValue(),
        "nginx:1.27.5",
      );
      assert.equal(
        (await api("/organizations/browser-team/spaces/dev/apps")).configuration
          .apps.length,
        0,
      );
      await page.goto(
        origin + "/?view=secret&organization=browser-team&space=dev",
      );
      await page
        .locator('.environment-group-settings input[name="name"]')
        .fill("browser-config");
      await page.locator('[name="variableName"]').first().fill("MESSAGE");
      await page
        .locator('[name="variableValue"]')
        .first()
        .fill("<script>literal</script>\nsecond line");
      await page
        .getByRole("button", { name: "Enter as YAML", exact: true })
        .click();
      assert.match(
        await page.locator('#environment-editor [name="values"]').inputValue(),
        /second line/,
      );
      await page
        .getByRole("button", { name: "Use key/value fields", exact: true })
        .click();
      assert.equal(
        await page.locator('[name="variableValue"]').first().inputValue(),
        "<script>literal</script>\nsecond line",
      );
      assert.equal(
        await page.locator('[name="baseVersion"]').inputValue(),
        "0",
      );
      await page.goto(
        origin + "/?view=custom-chart&organization=browser-team&space=dev",
      );
      await page
        .getByText("Install a custom addon", { exact: true })
        .first()
        .waitFor();
      await page.route("**/api/ui/custom-charts/defaults", (route) =>
        route.fulfill({
          json: {
            component: "custom-chart-defaults",
            data: {
              chart: { name: "fixture", version: "1.2.3" },
              values: "# Default comments\nreplicas: 1\n\n",
            },
          },
        }),
      );
      await page
        .locator('[name="repository"]')
        .fill("oci://registry.example/charts");
      await page.locator('[name="repository"]').press("Tab");
      await page.locator('input[name="chart"]').fill("fixture");
      await page.locator('input[name="version"]').fill("1.2.3");
      await page
        .getByRole("button", { name: "Show defaults", exact: true })
        .click();
      await page
        .getByRole("button", { name: "Copy Default values YAML", exact: true })
        .waitFor();
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
      await page
        .getByRole("button", { name: "Copy Default values YAML", exact: true })
        .click();
      assert.equal(
        await page.evaluate(() => window.copiedCode),
        "# Default comments\nreplicas: 1\n\n",
      );
      await page.unrouteAll({ behavior: "wait" });
      await verifyPostgres({ page, origin });
      await verifyInfrastructure({
        page,
        origin,
        api,
        kubeconfig,
        createOrganization,
      });
      await verifyRegistries({ page, origin, api });
      await verifySpaces({ page, origin, api });
      const viewResponse = await page.request.get(
        origin +
          "/api/ui/page?view=application&organization=browser-team&space=dev",
      );
      const telemetryPage = await viewResponse.json();
      telemetryPage.data.Application = {
        ...telemetryPage.data.ApplicationEditor.Draft,
        name: "web",
        image: "nginx:1.27.5",
      };
      telemetryPage.data.Draft = telemetryPage.data.Application;
      telemetryPage.data.Title = "web";
      telemetryPage.data.ApplicationTab = "observability";
      telemetryPage.data.ApplicationSubtab = "metrics";
      telemetryPage.data.Location =
        "/?view=application&organization=browser-team&space=dev&app=web&tab=observability";
      telemetryPage.data.Monitoring = {
        Organization: "browser-team",
        Space: "dev",
        Name: "web",
        Endpoint: "/api/ui/application",
        Path: "/api/ui/application?organization=browser-team&space=dev&app=web",
      };
      await page.route(
        (url) =>
          url.pathname === "/api/ui/page" &&
          url.searchParams.get("app") === "web",
        (route) => route.fulfill({ json: telemetryPage }),
      );
      let releaseInitial;
      const initial = new Promise((resolve) => {
        releaseInitial = resolve;
      });
      const chart = (range) => ({
        Title: "CPU usage",
        Unit: "mCPU",
        Latest: range === "3d" ? "11" : "9",
        Step: 30,
        Maximum: "12",
        Start: "Start",
        End: "End",
        Segments: [],
        Events: [],
        Samples:
          range === "30m"
            ? []
            : range === "3d"
              ? [{ X: 60, Y: 20, Time: "Newest", Value: "11" }]
              : [
                  { X: 10, Y: 100, Time: "Earlier", Value: "7" },
                  { X: 50, Y: 40, Time: "Later", Value: "9" },
                ],
      });
      await page.route(
        (url) => url.pathname === "/api/ui/application",
        async (route) => {
          const url = new URL(route.request().url());
          const range = url.searchParams.get("range") || "1h";
          if (range === "1h") await initial;
          try {
            await route.fulfill({
              json: {
                component: "application-metrics",
                data: {
                  Location: url.pathname + url.search,
                  Range: range,
                  Cpu: chart(range),
                  Memory: chart(range),
                },
              },
            });
          } catch {}
        },
      );
      await page.goto(origin + telemetryPage.data.Location);
      await page.locator('.range-controls button[value="30m"]').click();
      await page
        .locator('#application-metrics[data-selected-range="30m"]')
        .waitFor();
      releaseInitial();
      assert.equal(await page.locator("#application-metrics").count(), 1);
      assert.equal(
        await page
          .locator('.range-controls button[value="30m"]')
          .getAttribute("aria-pressed"),
        "true",
      );
      await page.locator('.range-controls button[value="12h"]').click();
      await page
        .locator('#application-metrics[data-selected-range="12h"]')
        .waitFor();
      await page.locator(".usage-chart").first().focus();
      await page.locator(".usage-chart").first().press("End");
      assert.match(
        await page.locator(".chart-tooltip").first().innerText(),
        /9 mCPU/,
      );
      await page.locator('.range-controls button[value="3d"]').click();
      await page
        .locator('#application-metrics[data-selected-range="3d"]')
        .waitFor();
      await page.locator(".usage-chart").first().focus();
      await page.locator(".usage-chart").first().press("End");
      assert.match(
        await page.locator(".chart-tooltip").first().innerText(),
        /11 mCPU/,
      );
      assert.equal(
        await page
          .locator('.range-controls button[value="3d"]')
          .getAttribute("aria-pressed"),
        "true",
      );
      await verifyPlanCopy({ page, origin, telemetryPage });
      await verifyManifestOnlyPlan({ page, origin, telemetryPage });
      await verifyDeploymentRecovery({ page, origin, telemetryPage });
      await verifyAppDeletion({ page, origin, telemetryPage });
      assert.deepEqual(
        failures,
        [],
        "The browser must not raise JavaScript errors.",
      );
    } catch (error) {
      if (page)
        console.error(
          JSON.stringify({
            url: page.url(),
            body: (await page.locator("body").innerText()).slice(0, 3000),
            errors: await page.evaluate(() => window.testErrors || []),
          }),
        );
      throw error;
    } finally {
      await browser?.close();
      if (server.exitCode === null) server.kill("SIGTERM");
      await exited;
      await new Promise((resolve) => cluster.close(resolve));
      await identity.close();
      await rm(directory, { recursive: true, force: true });
    }
  },
);
