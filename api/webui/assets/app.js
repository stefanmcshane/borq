function requestErrorTarget(element) {
  const form = element?.closest("form");
  if (!form) return document.getElementById("notice");
  let errors = form.querySelector(".form-errors");
  if (!errors) {
    errors = document.createElement("div");
    errors.className = "form-errors";
    errors.setAttribute("aria-live", "polite");
    (form.querySelector(".form-actions") || form).append(errors);
  }
  return errors;
}

function showRequestError(message, element) {
  const notice = requestErrorTarget(element);
  if (!notice) return;
  const alert = document.createElement("p");
  alert.className = "alert request-error";
  alert.setAttribute("data-variant", "destructive");
  alert.setAttribute("role", "alert");
  alert.textContent = message;
  notice.replaceChildren(alert);
}

function renderConfigurationDiffs() {
  document.querySelectorAll("pre.configuration-diff:not([data-highlighted])").forEach(function (diff) {
    const lines = diff.textContent.split("\n");
    if (!lines[0].startsWith("--- ") || !lines[1]?.startsWith("+++ ")) return;
    const body = document.createElement("span");
    body.className = "diff-lines";
    let before = 0, after = 0, added = 0, removed = 0;
    for (const line of lines.slice(2)) {
      const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
      if (hunk) {
        before = Number(hunk[1]);
        after = Number(hunk[2]);
        if (body.childElementCount) {
          const separator = document.createElement("span");
          separator.className = "diff-separator";
          separator.textContent = "⋯";
          body.append(separator);
        }
        continue;
      }
      if (!line) continue;
      const isAdded = line[0] === "+", isRemoved = line[0] === "-";
      const row = document.createElement("span");
      row.className = "diff-line" + (isAdded ? " diff-added" : isRemoved ? " diff-removed" : "");
      const values = [isAdded ? "" : String(before++), isRemoved ? "" : String(after++), isAdded ? "+" : isRemoved ? "−" : " ", line.slice(1)];
      values.forEach(function (value, index) {
        const cell = document.createElement("span");
        cell.className = index < 2 ? "diff-number" : index === 2 ? "diff-marker" : "diff-code";
        if (index < 2) cell.setAttribute("aria-hidden", "true");
        cell.textContent = value;
        row.append(cell);
      });
      body.append(row);
      if (isAdded) added++;
      if (isRemoved) removed++;
    }
    const header = document.createElement("span");
    header.className = "diff-header";
    const title = document.createElement("span");
    title.textContent = "Current → Planned";
    const summary = document.createElement("span");
    summary.className = "diff-summary";
    for (const [count, kind, sign] of [[added, "added", "+"], [removed, "removed", "−"]]) {
      const badge = document.createElement("span");
      badge.className = "diff-count-" + kind;
      badge.textContent = sign + count;
      badge.setAttribute("aria-label", count + " lines " + kind);
      summary.append(badge);
    }
    header.append(title, summary);
    diff.replaceChildren(header, body);
    diff.dataset.highlighted = "true";
    diff.tabIndex = 0;
    diff.setAttribute("role", "region");
    diff.setAttribute("aria-label", "Configuration changes");
  });
}

renderConfigurationDiffs();
document.addEventListener("htmx:load", renderConfigurationDiffs);

function renderInternalAddresses() {
  document.querySelectorAll("[data-internal-access]").forEach(function (panel) {
    const editor = panel.closest("#application-editor");
    const name = editor.querySelector('[name="name"]').value;
    const port = Number(editor.querySelector('[name="port"]').value);
    const namespace = panel.dataset.namespace;
    const valid = /^[a-z]([a-z0-9-]*[a-z0-9])?$/.test(name) && name.length <= 50 && Number.isInteger(port) && port > 0 && port <= 65535 && namespace;
    const address = panel.querySelector("[data-internal-address]");
    address.hidden = !valid;
    address.textContent = valid ? `http://${name}.${namespace}.svc:${port}` : "";
    panel.querySelector("[data-internal-title]").textContent = port === 0 ? "No network service" : "Internal access only";
    panel.querySelector("[data-internal-help]").textContent = port === 0 ? "Set a container port to give this app an internal address." : valid ? "Available after deployment to apps in this cluster. Add a public route for HTTPS access." : "Enter a valid application name and port to preview its internal address.";
  });
}

renderInternalAddresses();
document.addEventListener("htmx:load", renderInternalAddresses);
document.addEventListener("input", function (event) {
  if (event.target.closest("#application-editor") && (event.target.name === "name" || event.target.name === "port")) renderInternalAddresses();
});

document.addEventListener("htmx:beforeRequest", function (event) {
  event.detail.elt.closest("form")?.querySelector(".form-errors")?.replaceChildren();
});

document.addEventListener("htmx:beforeSwap", function (event) {
  if (!event.detail.isError) return;
  if (event.detail.xhr.getResponseHeader("HX-Retarget") !== "#notice") {
    event.detail.shouldSwap = false;
    return;
  }
  event.detail.target = requestErrorTarget(event.detail.requestConfig.elt);
});

document.addEventListener("htmx:sendError", function (event) {
  showRequestError("Couldn’t reach Borq. Check your connection and try again.", event.detail.elt);
});

document.addEventListener("htmx:responseError", function (event) {
  if (event.detail.xhr.getResponseHeader("HX-Retarget")) return;
  showRequestError("The request could not be completed. Please try again.", event.detail.elt);
});

document.addEventListener("htmx:afterSwap", function (event) {
  if (event.target.id !== "environment-editor" && event.target.id !== "application-editor") return;
  const focus = event.target.querySelector("[autofocus]") || event.target.querySelector('[name="values"], [name="variableName"], [name="configuration"], [name="name"]');
  focus?.focus();
});

document.addEventListener("htmx:responseError", function (event) {
  const checkbox = event.detail?.elt;
  if (checkbox?.name === "yamlMode") checkbox.checked = !checkbox.checked;
});

document.addEventListener("htmx:sendError", function (event) {
  const checkbox = event.detail?.elt;
  if (checkbox?.name === "yamlMode") checkbox.checked = !checkbox.checked;
});

document.addEventListener("click", function (event) {
  const add = event.target.closest("[data-environment-add]");
  if (add) {
    const editor = add.closest(".environment-editor");
    const rows = editor.querySelector("[data-environment-rows]");
    rows.append(editor.querySelector("[data-environment-template]").content.cloneNode(true));
    rows.lastElementChild.querySelector("input").focus();
    return;
  }
  const remove = event.target.closest("[data-environment-remove]");
  if (remove) {
    const row = remove.closest("[data-environment-row]");
    const next = row.nextElementSibling || row.previousElementSibling;
    const addButton = row.closest(".environment-editor").querySelector("[data-environment-add]");
    row.remove();
    (next ? next.querySelector("input") : addButton).focus();
  }
});
