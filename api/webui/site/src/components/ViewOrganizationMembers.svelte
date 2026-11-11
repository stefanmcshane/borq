<script lang="ts">
  import { submitForm } from "../lib/actions";
  import type { pageData } from "../lib/models";
  import RolePermissions from "./RolePermissions.svelte";
  export let data: pageData;
</script>

<div class="section-heading">
  <div>
    <h2>Organization members</h2>
    <p>Manage roles for people who have signed in to {data.Organization}.</p>
  </div>
</div>
<p class="field-help">
  Your identity provider controls who can sign in. Members appear after their
  first login with no assigned roles. Create roles below, then assign them to
  grant access to Borq. Manage invitations and accounts at your identity
  provider.
</p>

{#if !data.Members.length}
  <section class="card management-panel">
    <h3>No members yet</h3>
    <p>
      Members will appear here after signing in through the organization's
      provider.
    </p>
  </section>
{/if}

{#each data.Members as member (member.id)}
  <section
    class="card management-panel"
    aria-label={`Member ${member.email || member.subject}`}
  >
    <header class="panel-heading">
      <div>
        <h3>{member.email || member.subject}</h3>
        <p>
          {member.id === "initial-administrator"
            ? "Initial organization administrator"
            : "Organization member"}
        </p>
      </div>
      <span class="badge">{member.state}</span>
    </header>
    <p class="field-help">
      Provider identity: <span class="mono">{member.subject}</span> at {member.issuer}
    </p>
    {#if member.id !== "initial-administrator" && !member.emailVerified}
      <p class="field-help">
        {member.email
          ? "The provider has not verified this email."
          : "The provider did not supply an email address."} Confirm the provider
        identity before assigning roles.
      </p>
    {/if}
    {#if member.id === "initial-administrator"}
      <p class="field-help">
        Keep another administrator or the management recovery token available
        before removing this account's access.
      </p>
    {/if}
    {#if !member.roleIds.length}
      <p class="field-help">
        No roles assigned. This member has no organization access.
      </p>
    {/if}
    {#key `${member.id}:${member.revision}`}
      <form
        class="form-grid"
        method="post"
        action="/api/ui/actions"
        use:submitForm={{ target: "#workspace" }}
      >
        <input type="hidden" name="action" value="put-organization-member" />
        <input type="hidden" name="organization" value={data.Organization} />
        <input type="hidden" name="member" value={member.id} />
        <input type="hidden" name="revision" value={member.revision} />
        <fieldset class="form-grid two-columns">
          <legend>Roles</legend>
          {#each data.Roles as role (role.id)}
            <label class="checkbox-label">
              <input
                class="checkbox"
                type="checkbox"
                name="roleIds"
                value={role.id}
                checked={member.roleIds.includes(role.id)}
              />
              <span>{role.name}</span>
            </label>
          {/each}
        </fieldset>
        <label class="checkbox-label"
          ><input
            class="checkbox"
            type="checkbox"
            name="disabled"
            value="true"
            checked={member.state === "disabled"}
          />Disable access</label
        >
        <div class="form-actions">
          <button class="btn" type="submit">Save member roles</button>
        </div>
      </form>
    {/key}
  </section>
{/each}

<div class="section-heading">
  <div>
    <h2>Organization roles</h2>
    <p>
      Define permissions once and reuse them across members. Members with
      multiple roles receive their combined permissions.
    </p>
  </div>
</div>
<section class="card management-panel" aria-label="Create role">
  <h3>Create a role</h3>
  <form
    class="form-grid"
    method="post"
    action="/api/ui/actions"
    use:submitForm={{ target: "#workspace" }}
  >
    <input type="hidden" name="action" value="create-organization-role" />
    <input type="hidden" name="organization" value={data.Organization} />
    <label
      >Role name<input
        class="input"
        name="name"
        required
        maxlength="100"
        placeholder="e.g. Developer"
      /></label
    >
    <RolePermissions available={data.MemberPermissions} selected={[]} />
    <div class="form-actions">
      <button class="btn" type="submit">Create role</button>
    </div>
  </form>
</section>

{#each data.Roles as role (role.id)}
  <section class="card management-panel" aria-label={`Role ${role.name}`}>
    <h3>{role.name}</h3>
    {#key `${role.id}:${role.revision}`}
      <form
        class="form-grid"
        method="post"
        action="/api/ui/actions"
        use:submitForm={{ target: "#workspace" }}
      >
        <input type="hidden" name="action" value="put-organization-role" />
        <input type="hidden" name="organization" value={data.Organization} />
        <input type="hidden" name="role" value={role.id} />
        <input type="hidden" name="revision" value={role.revision} />
        <label
          >Role name<input
            class="input"
            name="name"
            required
            maxlength="100"
            value={role.name}
          /></label
        >
        <RolePermissions
          available={data.MemberPermissions}
          selected={role.permissions.includes("*")
            ? data.MemberPermissions
            : role.permissions}
        />
        <div class="form-actions">
          <button class="btn" type="submit">Save role</button>
        </div>
      </form>
    {/key}
    <form
      method="post"
      action="/api/ui/actions"
      use:submitForm={{ target: "#workspace" }}
    >
      <input type="hidden" name="action" value="delete-organization-role" />
      <input type="hidden" name="organization" value={data.Organization} />
      <input type="hidden" name="role" value={role.id} />
      <input type="hidden" name="revision" value={role.revision} />
      <button
        class="btn"
        data-variant="outline"
        type="submit"
        disabled={data.Members.some((member) =>
          member.roleIds.includes(role.id),
        )}>Delete role</button
      >
      {#if data.Members.some((member) => member.roleIds.includes(role.id))}<p
          class="field-help"
        >
          Remove this role from all members before deleting it.
        </p>{/if}
    </form>
  </section>
{/each}
