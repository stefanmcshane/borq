import type { infrastructureSelection } from "./models";

export function infrastructureUrl(
  selection: Partial<infrastructureSelection> = {},
) {
  const query = new URLSearchParams({
    view: "infrastructure",
    node: selection.Node || "management",
    tab: selection.Tab || "components",
  });
  const fields = {
    organization: selection.Organization,
    cluster: selection.Cluster,
    componentScope: selection.ComponentScope,
    component: selection.Component,
    managementOperation: selection.Operation,
  };
  for (const [name, value] of Object.entries(fields))
    if (value) query.set(name, value);
  return "/?" + query;
}
export function infrastructureTab(
  selection: infrastructureSelection,
  tab: string,
) {
  const next = { ...selection, Tab: tab };
  if (next.Node === "management") {
    const scope = tab === "settings" ? "postgres" : "management";
    if (scope !== next.ComponentScope) {
      next.ComponentScope = scope;
      next.Component = "";
    }
  }
  return infrastructureUrl(next);
}
