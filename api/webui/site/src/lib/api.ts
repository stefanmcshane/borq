import { z } from "zod";
import {
  ApiChartDefaultsSchema,
  ApiOperationSchema,
  applicationEditorDataSchema,
  applicationPanelDataSchema,
  clusterMetricsDataSchema,
  clusterSetupDataSchema,
  componentDataSchema,
  environmentEditorDataSchema,
  pageDataSchema,
  postgresPanelDataSchema,
} from "./models";

export const envelopeSchema = z.discriminatedUnion("component", [
  z.object({ component: z.literal("workspace"), data: pageDataSchema }),
  z.object({
    component: z.literal("application-editor"),
    data: applicationEditorDataSchema,
  }),
  z.object({
    component: z.literal("environment-editor"),
    data: environmentEditorDataSchema,
  }),
  z.object({ component: z.literal("ui-operation"), data: ApiOperationSchema }),
  z.object({
    component: z.literal("management-operation"),
    data: ApiOperationSchema,
  }),
  z.object({
    component: z.literal("postgres-operation"),
    data: ApiOperationSchema,
  }),
  z.object({
    component: z.literal("custom-chart-operation"),
    data: ApiOperationSchema,
  }),
  z.object({
    component: z.literal("ui-components"),
    data: componentDataSchema,
  }),
  z.object({
    component: z.literal("cluster-setup"),
    data: clusterSetupDataSchema,
  }),
  z.object({
    component: z.literal("cluster-metrics"),
    data: clusterMetricsDataSchema,
  }),
  z.object({
    component: z.literal("custom-chart-defaults"),
    data: ApiChartDefaultsSchema,
  }),
  z.object({
    component: z.literal("application-status"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("application-runtime"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("application-metrics"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("application-logs"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("application-deployments"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("application-events"),
    data: applicationPanelDataSchema,
  }),
  z.object({
    component: z.literal("postgres-metrics"),
    data: postgresPanelDataSchema,
  }),
  z.object({
    component: z.literal("postgres-logs"),
    data: postgresPanelDataSchema,
  }),
]);
export type Envelope = z.output<typeof envelopeSchema>;

let loadedVersion: string | undefined;

export async function request(
  url: string,
  options: RequestInit = {},
): Promise<Envelope> {
  const response = await fetch(url, {
    ...options,
    cache: "no-store",
    headers: { Accept: "application/json", ...options.headers },
  });
  if (response.status === 401) {
    window.location.assign("/login");
    throw new Error("Your session has expired. Sign in to continue.");
  }
  const version = response.headers.get("X-Borq-Ui-Version");
  if (version && loadedVersion && version !== loadedVersion) {
    window.location.reload();
    throw new Error("Borq has been updated. Reloading the workspace…");
  }
  if (version) loadedVersion = version;
  const body: unknown = await response.json();
  if (!response.ok) {
    const problem = z.object({ message: z.string() }).safeParse(body);
    throw new Error(
      problem.success
        ? problem.data.message
        : "The request could not be completed. Please try again.",
    );
  }
  return envelopeSchema.parse(body);
}
