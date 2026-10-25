const loadedUiVersion = document.querySelector('meta[name="borq-ui-version"]')?.content;
document.addEventListener('htmx:configRequest', function (event) {
  if (loadedUiVersion) event.detail.headers['X-Borq-Ui-Version'] = loadedUiVersion;
});

function requestErrorTarget(element) {
  const form = element?.closest("form");
  if (!form) return document.getElementById("notice");
  if (element.matches('[data-custom-chart-defaults]')) return form.querySelector('#custom-chart-defaults');
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
  const switchingFormat = event.detail.requestConfig?.elt?.name === "editorFormat";
  const focus = switchingFormat
    ? event.target.querySelector('[name="editorFormat"][aria-current="page"]')
    : event.target.querySelector("[autofocus]") || event.target.querySelector('[name="values"], [name="variableName"], [name="configuration"], [name="name"]');
  focus?.focus({preventScroll: switchingFormat});
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

function showSelectedHistoryRange() {
  const range = document.querySelector("#application-metrics")?.dataset.selectedRange || "1h";
  document.querySelectorAll(".range-controls button").forEach(function (button) {
    button.setAttribute("aria-pressed", String(button.value === range));
  });
}
showSelectedHistoryRange();
document.addEventListener("htmx:load", showSelectedHistoryRange);

document.addEventListener("change", function (event) {
  if (event.target.closest(".log-controls") && (event.target.name === "follow" || event.target.name === "range")) {
    event.target.form.requestSubmit();
  }
});

document.addEventListener("htmx:beforeSwap", function (event) {
  if (event.detail.target?.id !== "application-logs") return;
  const output = event.detail.target.querySelector("[data-log-output]");
  if (output) event.detail.xhr.borqLogPosition = {
    top: output.scrollTop,
    bottom: output.scrollHeight - output.clientHeight - output.scrollTop < 40
  };
});

document.addEventListener("htmx:afterSwap", function (event) {
  if (event.detail.target?.id !== "application-logs") return;
  const output = document.querySelector("#application-logs [data-log-output]");
  if (!output) return;
  const previous = event.detail.xhr?.borqLogPosition;
  if (previous && !previous.bottom) output.scrollTop = previous.top;
  else output.scrollTop = output.scrollHeight;
});

document.addEventListener("click", function (event) {
  if (!event.target.closest("[data-download-logs]")) return;
  const output = document.querySelector("[data-log-output]");
  if (!output) return;
  const lines = Array.from(output.querySelectorAll(".log-line, .log-event"), function (line) {
    return line.querySelector("time").dateTime + " " + (line.querySelector(".log-source")?.innerText || "event") + " " + (line.querySelector(".log-message, .log-event-copy, .log-event-message")?.textContent || "");
  });
  const address = URL.createObjectURL(new Blob([lines.join("\n") + "\n"], {type: "text/plain;charset=utf-8"}));
  const link = document.createElement("a");
  link.href = address;
  link.download = "application-logs.txt";
  link.click();
  setTimeout(function () { URL.revokeObjectURL(address); }, 1000);
});

function updateLogDownload() {
  const button = document.querySelector("[data-download-logs]");
  if (button) button.disabled = !document.querySelector("[data-log-output] .log-line, [data-log-output] .log-event");
}
updateLogDownload();
document.addEventListener("htmx:load", updateLogDownload);

const interactiveUsageCharts = new WeakSet();
function initializeUsageCharts() {
  document.querySelectorAll('[data-usage-chart]').forEach(function (stage) {
    if (interactiveUsageCharts.has(stage)) return;
    interactiveUsageCharts.add(stage);
    const svg = stage.querySelector('svg');
    const points = Array.from(stage.querySelectorAll('[data-chart-point]'));
    const events = Array.from(stage.querySelectorAll('[data-chart-event]'));
    const tooltip = stage.querySelector('.chart-tooltip');
    const cursor = stage.querySelector('.chart-cursor');
    let pointIndex = Math.max(0, points.length - 1), eventIndex = 0;
    function show(point, markers, x) {
      const nearby = markers.slice().sort(function (a, b) { return Math.abs(Number(a.dataset.x) - x) - Math.abs(Number(b.dataset.x) - x); });
      const lines = [];
      if (point) lines.push(point.dataset.time, point.dataset.value + ' ' + stage.dataset.unit);
      else if (nearby.length) lines.push(nearby[0].dataset.time);
      else lines.push('No measurement at this time');
      const summaries = new Map();
      nearby.forEach(function (marker) {
        if (!summaries.has(marker.dataset.message)) summaries.set(marker.dataset.message, marker);
      });
      Array.from(summaries.values()).slice(0, 3).forEach(function (marker) { lines.push(marker.dataset.clock + ' · ' + marker.dataset.message); });
      if (summaries.size > 3) lines.push('+' + (summaries.size - 3) + ' more · see timeline below');
      tooltip.replaceChildren(...lines.map(function (text) { const row = document.createElement('div'); row.textContent = text; return row; }));
      tooltip.hidden = false;
      tooltip.style.left = Math.max(0, Math.min(stage.clientWidth - tooltip.offsetWidth, x / 600 * stage.clientWidth)) + 'px';
      tooltip.style.transform = 'none';
      cursor.removeAttribute('hidden');
      cursor.querySelector('line').setAttribute('x1', x);
      cursor.querySelector('line').setAttribute('x2', x);
      const dot = cursor.querySelector('circle');
      dot.style.display = point ? '' : 'none';
      if (point) { dot.setAttribute('cx', point.getAttribute('cx')); dot.setAttribute('cy', point.getAttribute('cy')); }
    }
    function hide() { tooltip.hidden = true; cursor.setAttribute('hidden', ''); }
    svg.addEventListener('pointermove', function (event) {
      const matrix = svg.getScreenCTM();
      if (!matrix) return;
      const x = Math.max(0, Math.min(600, new DOMPoint(event.clientX, event.clientY).matrixTransform(matrix.inverse()).x));
      const point = points.reduce(function (closest, item) { return !closest || Math.abs(Number(item.getAttribute('cx')) - x) < Math.abs(Number(closest.getAttribute('cx')) - x) ? item : closest; }, null);
      const available = point && Math.abs(Number(point.getAttribute('cx')) - x) <= Math.max(3, Number(stage.dataset.step) * 0.75);
      show(available ? point : null, events.filter(function (marker) { return Math.abs(Number(marker.dataset.x) - x) < 8; }), x);
    });
    svg.addEventListener('pointerleave', hide);
    svg.addEventListener('blur', hide);
    svg.addEventListener('focus', function () {
      const point = points[pointIndex];
      if (point) show(point, [], Number(point.getAttribute('cx')));
      else if (events.length) show(null, [events[0]], Number(events[0].dataset.x));
    });
    svg.addEventListener('keydown', function (event) {
      if (event.key === 'Escape') { hide(); return; }
      if (['ArrowUp', 'ArrowDown'].includes(event.key) && events.length) {
        event.preventDefault();
        eventIndex = Math.max(0, Math.min(events.length - 1, eventIndex + (event.key === 'ArrowUp' ? -1 : 1)));
        show(null, [events[eventIndex]], Number(events[eventIndex].dataset.x));
      }
      if (!points.length || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      pointIndex = event.key === 'Home' ? 0 : event.key === 'End' ? points.length - 1 : Math.max(0, Math.min(points.length - 1, pointIndex + (event.key === 'ArrowLeft' ? -1 : 1)));
      const point = points[pointIndex], x = Number(point.getAttribute('cx'));
      show(point, events.filter(function (marker) { return Math.abs(Number(marker.dataset.x) - x) < 8; }), x);
    });
  });
}
initializeUsageCharts();
document.addEventListener('htmx:load', initializeUsageCharts);

document.addEventListener("htmx:historyRestore", initializeUsageCharts);

function revealSelectedApplicationTab() {
  document.querySelectorAll('.application-tabs').forEach(function (navigation) {
    const selected = navigation.querySelector('[aria-current="page"]');
    if (!selected) return;
    const container = navigation.getBoundingClientRect(), tab = selected.getBoundingClientRect();
    if (tab.left < container.left) navigation.scrollLeft += tab.left - container.left;
    else if (tab.right > container.right) navigation.scrollLeft += tab.right - container.right;
  });
}
revealSelectedApplicationTab();
document.addEventListener('htmx:load', revealSelectedApplicationTab);


// Preserve expanded deployment rows when their history or logs refresh.
document.addEventListener('htmx:beforeSwap', function (event) {
  if (!event.detail.xhr || event.detail.isError) return;
  event.detail.xhr.borqExpandedDeployments = Array.from(event.detail.target.querySelectorAll('[data-deployment-details][open]'), function (details) { return details.id; });
});
document.addEventListener('htmx:afterSwap', function (event) {
  for (const id of event.detail.xhr?.borqExpandedDeployments || []) {
    const details = document.getElementById(id);
    if (details?.matches('[data-deployment-details]')) details.open = true;
  }
});

// Reading an inline operation must not trigger the workspace's completion refresh.
document.addEventListener('deployment-complete', function (event) {
  if (event.target.closest('.inline-operation')) event.stopPropagation();
}, true);

const customChartEditors = new WeakSet();
function initializeCustomChartEditors() {
  document.querySelectorAll('[data-custom-chart-editor]').forEach(function (form) {
    if (customChartEditors.has(form)) return;
    customChartEditors.add(form);
    const repository = form.elements.namedItem('repository');
    const status = form.querySelector('#custom-chart-status');
    const indicator = form.querySelector('[data-repository-indicator]');
    const versionIndicator = form.querySelector('[data-version-indicator]');
    const versionStatus = form.querySelector('#custom-chart-version-status');
    const defaults = form.querySelector('[data-custom-chart-defaults]');
    const defaultsPopout = form.querySelector('#custom-chart-defaults-popout');
    const defaultsContent = form.querySelector('#custom-chart-defaults');
    const save = form.querySelector('[data-custom-chart-save]');
    let charts = [], pending, versionsPending, timer, requestedRepository, defaultsRequest, defaultsReference;
    const field = name => form.elements.namedItem(name);
    const reference = () => JSON.stringify([repository.value.trim(), field('chart').value, field('version').value]);
    function focusDefaults() {
      if (defaultsPopout.matches(':popover-open') && !defaultsPopout.contains(document.activeElement)) defaultsPopout.querySelector('[autofocus]').focus({preventScroll: true});
    }
    defaultsPopout.addEventListener('toggle', focusDefaults);
    function clearDefaults() {
      defaultsRequest?.abort();
      defaultsRequest = undefined;
      defaultsReference = undefined;
      if (defaultsPopout.matches(':popover-open')) defaultsPopout.hidePopover();
      defaultsContent.replaceChildren();
    }

    function showIndicator(element, state) {
      const paths = {
        loading: 'M20 7v5h-5M4 17v-5h5M6.1 8a7 7 0 0 1 11.5-2L20 9M4 15l2.4 3A7 7 0 0 0 17.9 16',
        loaded: 'm5 12 4 4L19 6',
        failed: 'm12 3 10 18H2L12 3ZM12 9v4m0 4h.01'
      };
      element.dataset.state = state;
      element.hidden = !paths[state];
      element.querySelector('path').setAttribute('d', paths[state] || '');
    }

    function showRepositoryStatus(state, message) {
      showIndicator(indicator, state);
      status.textContent = message;
    }

    function showVersionStatus(state, message) {
      showIndicator(versionIndicator, state);
      versionStatus.textContent = message;
    }

    function updateActions() {
      const ready = !pending && !versionsPending && repository.value.trim() && field('chart').value.trim() && field('version').value.trim();
      defaults.disabled = !ready;
      save.disabled = !ready;
    }

    function input(name, value) {
      const control = document.createElement('input');
      control.className = 'input';
      control.name = name;
      control.required = true;
      control.autocomplete = 'off';
      control.value = value;
      control.placeholder = name === 'chart' ? 'redis' : '1.2.3';
      field(name).replaceWith(control);
    }

    function select(name, values, value) {
      const control = document.createElement('select');
      control.className = 'select';
      control.name = name;
      control.required = true;
      control.add(new Option('Choose a ' + (name === 'chart' ? 'chart' : 'version'), ''));
      values.forEach(item => control.add(new Option(item, item)));
      if (value && !values.includes(value)) control.add(new Option(value + ' (saved selection)', value));
      control.value = value;
      field(name).replaceWith(control);
    }

    async function chooseVersions(value = '') {
      versionsPending?.abort();
      versionsPending = undefined;
      clearDefaults();
      select('version', [], '');
      const chart = field('chart').value;
      if (!chart) {
        field('version').disabled = true;
        showVersionStatus('idle', 'Choose a chart to load versions.');
        updateActions();
        return;
      }
      const controller = new AbortController();
      versionsPending = controller;
      field('version').disabled = true;
      showVersionStatus('loading', 'Loading versions…');
      updateActions();
      try {
        const parameters = new URLSearchParams({repository: repository.value.trim(), chart});
        const response = await fetch('/ui/custom-charts/versions?' + parameters, {signal: controller.signal, cache: 'no-store'});
        const versions = await response.json();
        if (!response.ok) throw new Error(versions.message || 'Could not load versions.');
        if (controller.signal.aborted) return;
        if (!versions.length) throw new Error('No versions were found for this chart.');
        select('version', versions, value);
        showVersionStatus('loaded', versions.length + (versions.length === 1 ? ' version loaded.' : ' versions loaded.'));
      } catch (error) {
        if (controller.signal.aborted) return;
        input('version', value);
        showVersionStatus('failed', error.message + ' Enter an exact version or select the chart again to retry.');
      } finally {
        if (versionsPending === controller) {
          versionsPending = undefined;
          field('version').disabled = false;
          updateActions();
        }
      }
    }

    function reset() {
      clearTimeout(timer);
      pending?.abort();
      pending = undefined;
      versionsPending?.abort();
      versionsPending = undefined;
      form.removeAttribute('aria-busy');
      charts = [];
      select('chart', [], '');
      select('version', [], '');
      field('version').disabled = true;
      showVersionStatus('idle', 'Choose a chart to load versions.');
      clearDefaults();
      updateActions();
    }

    async function loadCharts(preserve) {
      clearTimeout(timer);
      const chart = preserve ? field('chart').value : '';
      const version = preserve ? field('version').value : '';
      reset();
      const address = repository.value.trim();
      requestedRepository = address;
      if (address.startsWith('oci://')) {
        input('chart', chart);
        input('version', version);
        showVersionStatus('manual', 'Enter an exact chart version.');
        showRepositoryStatus('manual', 'Chart listing is unavailable for OCI repositories. Enter the published chart name and exact version.');
        updateActions();
        return;
      }
      if (!address) {
        showRepositoryStatus('idle', 'Enter a repository to choose a chart and version.');
        return;
      }
      const controller = new AbortController();
      pending = controller;
      showRepositoryStatus('loading', 'Loading charts…');
      form.setAttribute('aria-busy', 'true');
      updateActions();
      try {
        const response = await fetch('/ui/custom-charts/repository?' + new URLSearchParams({repository: address}), {signal: controller.signal, cache: 'no-store'});
        const result = await response.json();
        if (!response.ok) throw new Error(result.message || 'Could not load charts.');
        if (controller.signal.aborted) return;
        if (!result.length) throw new Error('No installable charts were found in this repository.');
        charts = result;
        select('chart', charts.map(chart => chart.name), chart);
        chooseVersions(version);
        showRepositoryStatus('loaded', charts.length + (charts.length === 1 ? ' chart loaded. Choose a version after selecting the chart.' : ' charts loaded. Choose a chart, then a version.'));
      } catch (error) {
        if (controller.signal.aborted) return;
        input('chart', chart);
        input('version', version);
        showVersionStatus('manual', 'Enter an exact chart version.');
        showRepositoryStatus('failed', error.message + ' Enter the chart and version manually, or edit the repository URL to retry.');
      } finally {
        if (pending === controller) {
          pending = undefined;
          form.removeAttribute('aria-busy');
          updateActions();
        }
      }
    }

    repository.addEventListener('input', function () {
      reset();
      requestedRepository = undefined;
      if (!repository.value.trim()) {
        showRepositoryStatus('idle', 'Enter a repository to choose a chart and version.');
        return;
      }
      showRepositoryStatus('loading', 'Loading charts…');
      timer = setTimeout(() => loadCharts(false), 500);
    });
    repository.addEventListener('change', function () {
      if (requestedRepository !== repository.value.trim()) loadCharts(false);
    });
    form.addEventListener('change', function (event) {
      if (event.target.name === 'chart' && event.target.tagName === 'SELECT') chooseVersions();
      if (event.target.name === 'chart' || event.target.name === 'version') clearDefaults();
      updateActions();
    });
    form.addEventListener('input', function (event) {
      if (event.target.name === 'chart' || event.target.name === 'version') clearDefaults();
      updateActions();
    });
    form.addEventListener('htmx:beforeRequest', function (event) {
      if (event.detail.elt !== defaults) return;
      defaultsPopout.showPopover();
      if (defaultsReference === reference() && defaultsContent.querySelector('pre')) {
        event.preventDefault();
        return;
      }
      defaultsRequest = event.detail.xhr;
      defaultsRequest.borqChartReference = reference();
      defaultsContent.textContent = 'Loading defaults…';
    });
    form.addEventListener('htmx:beforeSwap', function (event) {
      if (event.detail.target?.id === 'custom-chart-defaults' && event.detail.xhr.borqChartReference !== reference()) event.detail.shouldSwap = false;
    });
    form.addEventListener('htmx:afterSwap', function (event) {
      if (event.detail.target?.id !== 'custom-chart-defaults') return;
      if (event.detail.xhr.borqChartReference === reference() && defaultsContent.querySelector('pre')) defaultsReference = reference();
      focusDefaults();
    });
    form.addEventListener('htmx:afterRequest', function (event) {
      if (event.detail.xhr === defaultsRequest) defaultsRequest = undefined;
    });
    form.addEventListener('click', async function (event) {
      if (!event.target.closest('[data-copy-chart-defaults]')) return;
      const values = defaultsContent.querySelector('pre');
      const message = defaultsContent.querySelector('[data-defaults-copy-status]');
      try {
        await navigator.clipboard.writeText(values.textContent);
        message.textContent = 'Defaults copied.';
      } catch {
        values.focus();
        window.getSelection().selectAllChildren(values);
        message.textContent = 'Defaults selected. Use your browser’s Copy command.';
      }
    });
    loadCharts(true);
  });
}
initializeCustomChartEditors();
document.addEventListener('htmx:load', initializeCustomChartEditors);
document.addEventListener('htmx:historyRestore', initializeCustomChartEditors);

// Breadcrumb menus use native disclosures, with one menu open at a time.
document.addEventListener("click", function (event) {
  const selected = event.target.closest(".breadcrumb-dropdown");
  document.querySelectorAll(".breadcrumb-dropdown[open]").forEach(function (menu) {
    if (menu !== selected || event.target.closest("a")) menu.open = false;
  });
});

document.addEventListener("keydown", function (event) {
  if (event.key !== "Escape") return;
  const menu = document.querySelector(".breadcrumb-dropdown[open]");
  if (!menu) return;
  menu.open = false;
  menu.querySelector("summary").focus();
  event.preventDefault();
});

function openSetupDialog() {
  const dialog = document.querySelector("dialog[data-auto-open]");
  if (dialog && !dialog.open) {
    dialog.showModal();
    dialog.removeAttribute("data-auto-open");
  }
}
openSetupDialog();
document.addEventListener("htmx:load", openSetupDialog);

document.addEventListener("click", function (event) {
  const opener = event.target.closest("[data-dialog-open]");
  if (opener) {
    document.querySelectorAll(".breadcrumb-dropdown[open]").forEach(function (menu) { menu.open = false; });
    document.getElementById(opener.dataset.dialogOpen)?.showModal();
  }
  if (event.target.closest("[data-dialog-close]")) event.target.closest("dialog").close();
});

function lowercasePostgresName(event) {
  const input = event.target;
  if (event.isComposing || !input.matches('[data-postgres-name]')) return;
  const value = input.value.replace(/[A-Z]/g, function (character) { return character.toLowerCase(); });
  if (value === input.value) return;
  const start = input.selectionStart, end = input.selectionEnd, direction = input.selectionDirection;
  input.value = value;
  input.setSelectionRange(start, end, direction);
}
document.addEventListener('input', lowercasePostgresName);
document.addEventListener('compositionend', lowercasePostgresName);

document.addEventListener("change", function (event) {
  if (event.target.matches("select[data-settings-navigation]")) {
    htmx.ajax("GET", event.target.value, {source: event.target, target: "#workspace", swap: "outerHTML"});
  }
});
