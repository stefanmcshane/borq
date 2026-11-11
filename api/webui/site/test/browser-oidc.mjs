import assert from "node:assert/strict";
import { createHash } from "node:crypto";

// Browser policy permits real signed organization login and provider logout while
// keeping recovery credentials on Borq, even when the provider is unavailable.
export async function verifyOrganizationSignIn({ page, origin, issuer }) {
  const context = await page.context().browser().newContext();
  const loginPage = await context.newPage();
  loginPage.setDefaultTimeout(10000);
  const providerOrigin = new URL(issuer).origin;
  const signIn = async () => {
    await loginPage.goto(origin + "/login");
    await loginPage
      .getByLabel("Organization", { exact: true })
      .selectOption("browser-team");
    await loginPage
      .getByRole("button", { name: "Continue to sign in", exact: true })
      .click();
    await loginPage.waitForURL(
      (url) =>
        url.origin === providerOrigin && url.pathname === "/oidc/authorize",
    );
    await loginPage
      .getByRole("heading", { name: "Identity provider sign-in", exact: true })
      .waitFor();
    await loginPage
      .getByRole("button", {
        name: "Sign in as browser administrator",
        exact: true,
      })
      .click();
    await loginPage.waitForURL(
      (url) =>
        url.origin === origin &&
        url.pathname === "/" &&
        url.searchParams.get("organization") === "browser-team",
    );
    await loginPage.goto(origin + "/account");
    await loginPage
      .getByRole("heading", { name: "Account", exact: true })
      .waitFor();
  };
  try {
    await signIn();
    await loginPage.goto(
      origin + "/?view=organization-authentication&organization=browser-team",
    );
    await loginPage
      .getByRole("heading", { name: "OIDC provider", exact: true })
      .waitFor();
    const authenticationResponse = await loginPage.request.get(
      origin +
        "/api/ui/page?view=organization-authentication&organization=browser-team",
    );
    assert.equal(authenticationResponse.status(), 200);
    const authentication = (await authenticationResponse.json()).data;
    assert.equal(authentication.ManagementAdmin, false);
    assert.equal(authentication.PocketIdConfiguration, undefined);
    assert.equal(
      await loginPage.getByLabel("Client secret", { exact: true }).count(),
      0,
    );
    assert.equal(
      await loginPage
        .getByRole("button", { name: "Save and check discovery", exact: true })
        .count(),
      0,
    );
    assert.equal(
      await loginPage
        .getByRole("button", {
          name: "Set up administrator passkey",
          exact: true,
        })
        .count(),
      0,
    );
    await loginPage
      .getByText(
        "Provider configuration requires management administrator access.",
        { exact: true },
      )
      .waitFor();
    await loginPage.goto(origin + "/account");
    const providerLogout = loginPage.waitForRequest((request) => {
      const url = new URL(request.url());
      return (
        url.origin === providerOrigin && url.pathname === "/oidc/end-session"
      );
    });
    await loginPage
      .getByRole("button", { name: "Sign out", exact: true })
      .click({ noWaitAfter: true });
    const logoutRequest = await providerLogout;
    const logoutQuery = new URL(logoutRequest.url()).searchParams;
    assert.equal(logoutQuery.get("client_id"), "browser-team-client");
    assert.equal(
      logoutQuery.get("post_logout_redirect_uri"),
      origin + "/login",
    );
    assert.ok(
      Boolean(logoutQuery.get("id_token_hint")),
      "Provider logout requires the issued ID token hint",
    );
    assert.equal(logoutRequest.headers()["referer"], undefined);
    await loginPage.waitForURL(
      (url) =>
        url.origin === origin &&
        url.pathname === "/login" &&
        url.searchParams.get("provider_logout") === "complete",
    );
    await loginPage.goto(origin + "/account");
    await loginPage.waitForURL(origin + "/login");

    await signIn();
    const blockedLogout = loginPage.waitForEvent("requestfailed", {
      predicate: (request) => {
        const url = new URL(request.url());
        return (
          url.origin === providerOrigin && url.pathname === "/oidc/end-session"
        );
      },
    });
    await loginPage.route(providerOrigin + "/oidc/end-session**", (route) =>
      route.abort("failed"),
    );
    await loginPage
      .getByRole("button", { name: "Sign out", exact: true })
      .click({ noWaitAfter: true });
    await blockedLogout;
    // Chromium may still be committing its provider error document. A second
    // tab shares the same cookie jar and can independently check revocation.
    const recoveryPage = await loginPage.context().newPage();
    recoveryPage.setDefaultTimeout(10000);
    await recoveryPage.goto(origin + "/account");
    await recoveryPage.waitForURL(origin + "/login");
    await loginPage.unroute(providerOrigin + "/oidc/end-session**");

    await recoveryPage.goto(origin + "/login/admin");
    await recoveryPage.evaluate((destination) => {
      window.securityViolations = [];
      document.addEventListener("securitypolicyviolation", (event) => {
        window.securityViolations.push(event.effectiveDirective);
      });
      document.querySelector("form").action = destination;
    }, issuer + "/authorize");
    await recoveryPage
      .getByLabel("Admin token", { exact: true })
      .fill("browser-test-dummy-token");
    await recoveryPage
      .getByRole("button", { name: "Continue as administrator", exact: true })
      .click({ noWaitAfter: true });
    await recoveryPage.waitForFunction(() =>
      window.securityViolations.includes("form-action"),
    );
    assert.equal(recoveryPage.url(), origin + "/login/admin");
  } finally {
    await context.close();
  }
}

// Browser coverage owns callback previews and secret-field isolation; Go tests own storage and the OIDC exchange.
export async function verifyOrganizationOidc({
  page,
  origin,
  api,
  createOrganization,
}) {
  await createOrganization("oidc-other");
  const otherProvider = await api("/organizations/oidc-other/oidc");
  assert.equal(
    otherProvider.logoutCallbackUrl,
    otherProvider.borqPublicUrl + "/login",
  );
  const organizations = await api("/organizations");
  const organization = organizations.find(
    (item) => item.name === "browser-team",
  );
  assert.ok(organization?.id);
  await page.getByRole("link", { name: "Authentication", exact: true }).click();
  await page
    .getByRole("heading", { name: "OIDC provider", exact: true })
    .waitFor();
  assert.equal(new URL(page.url()).searchParams.get("space"), null);
  const logoutCallback = page.getByLabel(
    "Logout callback URL to register at your provider",
    { exact: true },
  );
  assert.equal(await logoutCallback.inputValue(), origin + "/login");
  await page.getByLabel("Issuer URL", { exact: true }).fill("");
  const oidcSettings = page.getByRole("form", {
    name: "OIDC provider settings",
    exact: true,
  });
  const administratorEmail = oidcSettings.getByLabel("Administrator email", {
    exact: true,
  });
  assert.equal(
    await administratorEmail.inputValue(),
    "browser-admin@example.test",
  );
  assert.equal(await administratorEmail.getAttribute("required"), "");
  await oidcSettings
    .getByLabel("Borq public URL", { exact: true })
    .fill("https://borq-public.example.test/");
  assert.equal(
    await logoutCallback.inputValue(),
    "https://borq-public.example.test/login",
  );
  assert.equal(await logoutCallback.getAttribute("readonly"), "");
  await oidcSettings
    .getByLabel("Borq public URL", { exact: true })
    .fill(origin);
  const issuer = "https://id.example.test/tenant";
  await page.getByLabel("Issuer URL", { exact: true }).fill(issuer);
  const expected = `${origin}/api/auth/oidc/callback/${createHash("sha256").update(`${organization.id}/${issuer}`).digest("hex").slice(0, 32)}`;
  await page.waitForFunction(
    (expected) =>
      Array.from(document.querySelectorAll("input[readonly]")).some(
        (input) => input.value === expected,
      ),
    expected,
  );
  assert.equal(
    await page
      .getByLabel("Callback URL to register at your provider", { exact: true })
      .inputValue(),
    expected,
  );
  const secret = page.getByLabel("Client secret", { exact: true });
  assert.equal(await secret.getAttribute("type"), "password");
  await secret.fill("unsaved-private-secret");
  assert.equal(
    await page
      .getByRole("button", { name: "Test saved sign-in settings", exact: true })
      .isDisabled(),
    false,
  );
  await page.getByLabel("Switch organization", { exact: true }).click();
  await page
    .getByRole("navigation", { name: "Breadcrumb" })
    .getByRole("link", { name: "oidc-other", exact: true })
    .click();
  await page.getByRole("link", { name: "Authentication", exact: true }).click();
  await page
    .getByRole("heading", { name: "OIDC provider", exact: true })
    .waitFor();
  assert.equal(await secret.inputValue(), "");
  assert.equal(
    await page.getByLabel("Issuer URL", { exact: true }).inputValue(),
    otherProvider.issuerUrl,
  );
  assert.equal(
    await logoutCallback.inputValue(),
    otherProvider.logoutCallbackUrl,
  );
  assert.equal(
    await page
      .getByRole("heading", { name: "Managed Pocket ID", exact: true })
      .count(),
    0,
  );
  await page.goto(origin + "/?view=overview&organization=browser-team");
}
