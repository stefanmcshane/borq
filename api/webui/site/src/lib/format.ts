import { stringify } from "yaml";

export function truth<T>(value: T): value is NonNullable<T> {
  return typeof value === "object" && value !== null
    ? Object.keys(value).length > 0
    : Boolean(value);
}
export function pageUrl(
  view: string,
  organization: string,
  space: string,
  ...extra: string[]
) {
  if (view === "environment") view = "secrets";
  if (view === "environment-group") view = "secret";
  const query = new URLSearchParams({ view });
  if (organization) query.set("organization", organization);
  if (space && !["spaces", "clusters", "cluster"].includes(view))
    query.set("space", space);
  for (let index = 0; index + 1 < extra.length; index += 2)
    query.set(extra[index], extra[index + 1]);
  return "/?" + query;
}
export function appPanelUrl(
  organization: string,
  space: string,
  app: string,
  panel: string,
) {
  return (
    "/api/ui/application?" +
    new URLSearchParams({ organization, space, app, panel })
  );
}
export function yaml(value: unknown) {
  return stringify(value, { lineWidth: 0 });
}
export function terminal(status: string) {
  return ["succeeded", "failed", "canceled", "superseded"].includes(status);
}
export function variant(status: string) {
  if (["failed", "conflict"].includes(status)) return "destructive";
  if (["ready", "succeeded", "deployed"].includes(status)) return "secondary";
  return "outline";
}
export function componentLabel(value: string) {
  return value ? value[0].toUpperCase() + value.slice(1) : value;
}
export function formatTimestamp(value: string, pattern?: string) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return value;
  if (pattern?.startsWith("2006")) return date.toISOString();
  return date
    .toLocaleString("en-US", {
      month: "short",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
      timeZone: "UTC",
    })
    .replace(",", "");
}
export function timestampKey(value: string) {
  return value.replaceAll(/[^\d]/g, "");
}
export function timeLabel(value: string) {
  return value && !value.startsWith("0001-")
    ? new Date(value).toLocaleString("en-US", {
        month: "short",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
        timeZone: "UTC",
      }) + " UTC"
    : "—";
}
export function bytesLabel(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const index = Math.min(
    Math.floor(Math.log(value) / Math.log(1024)),
    units.length - 1,
  );
  return (value / 1024 ** index).toFixed(index ? 1 : 0) + " " + units[index];
}
