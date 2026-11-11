import assert from "node:assert/strict";

// Drive provider login and role assignment against the real server. The fixture
// signs identities; it does not simulate a passkey authenticator or SCIM.
export async function verifyOrganizationMembers({ page, origin, api, issuer }) {
  const context = await page.context().browser().newContext();
  const viewer = await context.newPage();
  viewer.setDefaultTimeout(10000);
  const location =
    origin + "/?view=organization-members&organization=browser-team";
  try {
    await viewer.goto(origin + "/login");
    await viewer
      .getByLabel("Organization", { exact: true })
      .selectOption("browser-team");
    await viewer
      .getByRole("button", { name: "Continue to sign in", exact: true })
      .click();
    await viewer.waitForURL(
      (url) =>
        url.origin === new URL(issuer).origin &&
        url.pathname === "/oidc/authorize",
    );
    await viewer
      .getByRole("button", { name: "Sign in as provider member", exact: true })
      .click();
    await viewer
      .getByRole("heading", { name: "Waiting for a role", exact: true })
      .waitFor();
    assert.equal(
      await viewer.getByRole("link", { name: "Members", exact: true }).count(),
      0,
    );

    await page.goto(location);
    await page
      .getByRole("heading", {
        name: "Organization members",
        level: 2,
        exact: true,
      })
      .waitFor();
    assert.equal(
      await page
        .getByRole("button", { name: "Invite member", exact: true })
        .count(),
      0,
    );
    const memberPanel = page.getByRole("region", {
      name: "Member browser-viewer@example.test",
      exact: true,
    });
    await memberPanel
      .getByText("No roles assigned. This member has no organization access.", {
        exact: true,
      })
      .waitFor();

    const creation = page.getByRole("region", {
      name: "Create role",
      exact: true,
    });
    await creation.getByLabel("Role name", { exact: true }).fill("Viewer");
    for (const permission of [
      "organization:read",
      "cluster:read",
      "space:read",
      "app:read",
    ]) {
      await creation.getByLabel(permission, { exact: true }).check();
    }
    await creation
      .getByRole("button", { name: "Create role", exact: true })
      .click();
    await page
      .getByRole("region", { name: "Role Viewer", exact: true })
      .waitFor();
    await memberPanel.getByLabel("Viewer", { exact: true }).check();
    await memberPanel
      .getByRole("button", { name: "Save member roles", exact: true })
      .click();
    await page.getByText("Member roles updated.", { exact: false }).waitFor();
    await viewer.reload();
    await viewer
      .getByRole("heading", {
        name: "Ready for your first application",
        exact: true,
      })
      .waitFor();
    assert.equal(
      (
        await viewer.request.get(
          origin +
            "/api/ui/page?view=organization-members&organization=browser-team",
        )
      ).status(),
      403,
    );

    // Permission checkboxes belong to roles, never individual member forms.
    assert.equal(
      await memberPanel.locator('input[name="permissions"]').count(),
      0,
    );
    const rolePanel = page.getByRole("region", {
      name: "Role Viewer",
      exact: true,
    });
    await rolePanel.getByLabel("app:read", { exact: true }).uncheck();
    await rolePanel
      .getByRole("button", { name: "Save role", exact: true })
      .click();
    await page.getByText("Role saved.", { exact: false }).waitFor();
    const members = await api("/organizations/browser-team/members");
    const member = members.find(
      (item) => item.email === "browser-viewer@example.test",
    );
    assert.ok(member);
    assert.equal(member.permissions.includes("app:read"), false);
    assert.equal(member.roleIds.length, 1);

    await memberPanel.getByLabel("Disable access", { exact: true }).check();
    await memberPanel
      .getByRole("button", { name: "Save member roles", exact: true })
      .click();
    await memberPanel.getByText("disabled", { exact: true }).waitFor();
    assert.equal(
      (
        await viewer.request.get(
          origin + "/api/ui/page?view=overview&organization=browser-team",
        )
      ).status(),
      401,
      "Disabling a member must revoke an already signed-in session",
    );
    await memberPanel.getByLabel("Disable access", { exact: true }).uncheck();
    await memberPanel
      .getByRole("button", { name: "Save member roles", exact: true })
      .click();
    await memberPanel.getByText("active", { exact: true }).waitFor();

    await memberPanel.getByLabel("Viewer", { exact: true }).uncheck();
    await memberPanel
      .getByRole("button", { name: "Save member roles", exact: true })
      .click();
    await memberPanel
      .getByText("No roles assigned. This member has no organization access.", {
        exact: true,
      })
      .waitFor();
    await rolePanel
      .getByRole("button", { name: "Delete role", exact: true })
      .click();
    await page.getByText("Role deleted.", { exact: true }).waitFor();
    assert.equal(await rolePanel.count(), 0);
  } finally {
    await context.close();
  }
}
