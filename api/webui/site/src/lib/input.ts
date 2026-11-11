const trimmedFields = new Set([
  "name",
  "image",
  "port",
  "replicas",
  "organization",
  "space",
  "cluster",
  "app",
  "application",
  "postgres",
  "spacePocketId",
  "issuerUrl",
  "adminUsername",
  "adminEmail",
  "storageClass",
  "certificateIssuer",
  "plan",
  "deployment",
  "backup",
  "group",
  "sourceName",
  "sourceVersion",
  "version",
  "baseVersion",
  "groupVersion",
  "environment",
  "environmentVersion",
  "environmentChoice",
  "variableName",
  "ingressName",
  "ingressHost",
  "ingressSubdomain",
  "ingressDomain",
  "ingressPath",
  "savedIngress",
  "serviceNamespace",
  "predeployTimeoutSeconds",
  "predeployBackoffLimit",
  "removeIngress",
  "repository",
  "repo",
  "chart",
  "region",
  "bucket",
  "prefix",
  "endpoint",
  "interval",
  "keepLast",
  "githubUsername",
  "recoveryTarget",
  "component",
  "componentScope",
  "componentOrganization",
  "componentCluster",
  "managementOperation",
  "view",
  "tab",
  "subtab",
  "node",
  "range",
  "end",
  "storage",
  "instances",
  "restoreSchedule",
]);

function trimsInput(element: Element | null): element is HTMLInputElement {
  return (
    element instanceof HTMLInputElement &&
    (trimmedFields.has(element.name || element.dataset.postgresSetting || "") ||
      /^(readiness|startup)(Path|Port|PeriodSeconds|TimeoutSeconds|FailureThreshold)$/.test(
        element.name,
      ))
  );
}

function updateInput(input: HTMLInputElement, value: string) {
  if (value === input.value) return;
  input.value = value;
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
}

export function trimInput(element: Element | null) {
  if (trimsInput(element)) updateInput(element, element.value.trim());
}

export function trimFormInputs(form: HTMLFormElement) {
  for (const input of form.querySelectorAll("input")) trimInput(input);
}

export function trimPastedInput(event: ClipboardEvent) {
  if (!(event.target instanceof Element) || !trimsInput(event.target)) return;
  const pasted = event.clipboardData?.getData("text/plain");
  if (pasted === undefined || pasted === pasted.trim()) return;
  const input = event.target;
  const start = input.selectionStart,
    end = input.selectionEnd;
  if (start === null || end === null) {
    // URL and number inputs hide their selection; let the browser retain it.
    if (document.execCommand("insertText", false, pasted.trim()))
      event.preventDefault();
    return;
  }
  const value =
    input.value.slice(0, start) + pasted.trim() + input.value.slice(end);
  event.preventDefault();
  updateInput(input, value);
  input.setSelectionRange(
    start + pasted.trim().length,
    start + pasted.trim().length,
  );
}
