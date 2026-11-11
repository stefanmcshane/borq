import { z } from "zod";

export const ApiCheckSchema = z.object({
  enabled: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  failureThreshold: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  path: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  periodSeconds: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  port: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  timeoutSeconds: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiCheck = z.output<typeof ApiCheckSchema>;

export const ApiChecksSchema = z.object({
  readiness: ApiCheckSchema.nullish().transform(
    (value) => value ?? ApiCheckSchema.parse({}),
  ),
  startup: ApiCheckSchema.nullish().transform(
    (value) => value ?? ApiCheckSchema.parse({}),
  ),
});
export type ApiChecks = z.output<typeof ApiChecksSchema>;

export const ApiEnvReferenceSchema = z.object({
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiEnvReference = z.output<typeof ApiEnvReferenceSchema>;

export const ApiExpandedIngressSchema = z.object({
  host: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  path: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiExpandedIngress = z.output<typeof ApiExpandedIngressSchema>;

export const ApiPredeploySchema = z.object({
  backoffLimit: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  command: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  timeoutSeconds: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiPredeploy = z.output<typeof ApiPredeploySchema>;

export const ApiSqliteSettingsSchema = z.object({
  enabled: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  backups: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
});

export const ApiAppSchema = z.object({
  sqlite: ApiSqliteSettingsSchema.nullish().transform(
    (value) => value ?? ApiSqliteSettingsSchema.parse({}),
  ),
  checks: ApiChecksSchema.nullish().transform(
    (value) => value ?? ApiChecksSchema.parse({}),
  ),
  command: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  env: ApiEnvReferenceSchema.nullish().transform(
    (value) => value ?? ApiEnvReferenceSchema.parse({}),
  ),
  image: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ingresses: z
    .array(
      ApiExpandedIngressSchema.nullish().transform(
        (value) => value ?? ApiExpandedIngressSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  port: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  predeploy: ApiPredeploySchema.nullish().transform(
    (value) => value ?? ApiPredeploySchema.parse({}),
  ),
  replicas: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiApp = z.output<typeof ApiAppSchema>;

export const applicationCheckDataSchema = z.object({
  Check: ApiCheckSchema.nullish().transform(
    (value) => value ?? ApiCheckSchema.parse({}),
  ),
  Description: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  PortSet: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Title: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type applicationCheckData = z.output<typeof applicationCheckDataSchema>;

export const applicationIngressDataSchema = z.object({
  Subdomain: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Domain: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  HostError: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Focus: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Route: ApiExpandedIngressSchema.nullish().transform(
    (value) => value ?? ApiExpandedIngressSchema.parse({}),
  ),
  Saved: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
});
export type applicationIngressData = z.output<
  typeof applicationIngressDataSchema
>;

export const ApiVariableSchema = z.object({
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  value: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiVariable = z.output<typeof ApiVariableSchema>;

export const applicationEditorDataSchema = z.object({
  RoutePolicyError: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  RoutePolicy: z
    .object({
      certificateIssuer: z
        .string()
        .nullish()
        .transform((value) => value ?? ""),
      domains: z
        .array(z.string())
        .nullish()
        .transform((value) => value ?? []),
    })
    .nullish()
    .transform((value) => value ?? { certificateIssuer: "", domains: [] }),
  Checks: z
    .array(
      applicationCheckDataSchema
        .nullish()
        .transform((value) => value ?? applicationCheckDataSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Document: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Draft: ApiAppSchema.nullish().transform(
    (value) => value ?? ApiAppSchema.parse({}),
  ),
  EnvironmentUrl: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ExistingName: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Format: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SecretDraft: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SecretValues: z
    .array(ApiVariableSchema)
    .nullish()
    .transform((value) => value ?? []),
  GroupNames: z
    .array(
      z
        .string()
        .nullish()
        .transform((value) => value ?? ""),
    )
    .nullish()
    .transform((value) => value ?? []),
  Ingresses: z
    .array(
      applicationIngressDataSchema
        .nullish()
        .transform((value) => value ?? applicationIngressDataSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SavedIngresses: z
    .array(
      z
        .string()
        .nullish()
        .transform((value) => value ?? ""),
    )
    .nullish()
    .transform((value) => value ?? []),
  Space: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SqliteRetained: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Yaml: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type applicationEditorData = z.output<
  typeof applicationEditorDataSchema
>;

export const ApiManifestSchema = z
  .record(z.unknown())
  .nullish()
  .transform((value) => value ?? {});
export type ApiManifest = z.output<typeof ApiManifestSchema>;

export const applicationSummarySchema = ApiAppSchema.pick({
  name: true,
  image: true,
  port: true,
  replicas: true,
  env: true,
});

export const configurationSummarySchema = z.object({
  apps: z
    .array(
      applicationSummarySchema
        .nullish()
        .transform((value) => value ?? applicationSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
});
export type configurationSummary = z.output<typeof configurationSummarySchema>;

export const applicationListDataSchema = z.object({
  configuration: configurationSummarySchema
    .nullish()
    .transform((value) => value ?? configurationSummarySchema.parse({})),
});
export type applicationListData = z.output<typeof applicationListDataSchema>;

export const spaceSummarySchema = z.object({
  cluster: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  configuration: configurationSummarySchema
    .nullish()
    .transform((value) => value ?? configurationSummarySchema.parse({})),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  lastSuccessfulRevision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type spaceSummary = z.output<typeof spaceSummarySchema>;

export const clusterSummarySchema = z.object({
  Context: z.string().default(""),
  ClientAPIServer: z.string().nullish(),
  InCluster: z.boolean().default(false),
  ApplicationCount: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Spaces: z
    .array(
      spaceSummarySchema
        .nullish()
        .transform((value) => value ?? spaceSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
});
export type clusterSummary = z.output<typeof clusterSummarySchema>;

export const ApiClusterSchema = z.object({
  clientApiServer: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  context: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  inCluster: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  kubeconfigPath: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiCluster = z.output<typeof ApiClusterSchema>;

export const ApiCustomChartInputSchema = z.object({
  chart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  logs: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false)
    .nullish()
    .transform((value) => value ?? null),
  podSelector: z
    .record(z.unknown())
    .nullish()
    .transform((value) => value ?? {})
    .nullish()
    .transform((value) => value ?? null),
  repository: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  values: z
    .record(z.unknown())
    .nullish()
    .transform((value) => value ?? {})
    .nullish()
    .transform((value) => value ?? null),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiCustomChartInput = z.output<typeof ApiCustomChartInputSchema>;

export const ApiCustomChartSchema = z.object({
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  operation: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  releaseName: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  specification: ApiCustomChartInputSchema.nullish().transform(
    (value) => value ?? ApiCustomChartInputSchema.parse({}),
  ),
  state: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  updatedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiCustomChart = z.output<typeof ApiCustomChartSchema>;

export const customChartEditorDataSchema = z.object({
  Chart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Existing: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Logs: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Repository: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Selector: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Values: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type customChartEditorData = z.output<
  typeof customChartEditorDataSchema
>;

export const environmentEditorDataSchema = z.object({
  Format: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Values: z
    .array(
      ApiVariableSchema.nullish().transform(
        (value) => value ?? ApiVariableSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  Yaml: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type environmentEditorData = z.output<
  typeof environmentEditorDataSchema
>;

export const ApiEnvironmentGroupSchema = z.object({
  managedBy: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  values: z
    .array(
      ApiVariableSchema.nullish().transform(
        (value) => value ?? ApiVariableSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  version: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiEnvironmentGroup = z.output<typeof ApiEnvironmentGroupSchema>;

export const environmentGroupSummarySchema = z.object({
  type: z
    .string()
    .nullish()
    .transform((value) => value ?? "secret-group"),
  managedBy: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  variableCount: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  version: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type environmentGroupSummary = z.output<
  typeof environmentGroupSummarySchema
>;

export const monitoringViewDataSchema = z.object({
  Endpoint: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  LogsDescription: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  LogsTitle: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Path: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Space: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type monitoringViewData = z.output<typeof monitoringViewDataSchema>;

export const ApiOperationPhaseSchema = z
  .string()
  .nullish()
  .transform((value) => value ?? "");
export type ApiOperationPhase = z.output<typeof ApiOperationPhaseSchema>;

export const ApiResourceStatusSchema = z.object({
  apiVersion: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  kind: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  state: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiResourceStatus = z.output<typeof ApiResourceStatusSchema>;

export const ApiOperationStatusSchema = z
  .string()
  .nullish()
  .transform((value) => value ?? "");
export type ApiOperationStatus = z.output<typeof ApiOperationStatusSchema>;

export const ApiOperationSchema = z.object({
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  phase: ApiOperationPhaseSchema,
  planId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  resources: z
    .array(
      ApiResourceStatusSchema.nullish().transform(
        (value) => value ?? ApiResourceStatusSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  spaceId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  startedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  status: ApiOperationStatusSchema,
  updatedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiOperation = z.output<typeof ApiOperationSchema>;

export const ApiOrganizationSchema = z.object({
  authenticationState: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  managedAddons: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiOrganization = z.output<typeof ApiOrganizationSchema>;

export const ApiResourceDiffActionSchema = z
  .string()
  .nullish()
  .transform((value) => value ?? "");
export type ApiResourceDiffAction = z.output<
  typeof ApiResourceDiffActionSchema
>;

export const ApiResourceDiffSchema = z.object({
  apiVersion: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  action: ApiResourceDiffActionSchema,
  desired: ApiManifestSchema,
  fingerprint: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  kind: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  live: ApiManifestSchema.nullish().transform((value) => value ?? null),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiResourceDiff = z.output<typeof ApiResourceDiffSchema>;

export const resourceDiffSummarySchema = ApiResourceDiffSchema.pick({
  kind: true,
  name: true,
  action: true,
});

export const planDataSchema = z.object({
  baseRevision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  configurationDiff: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  manifests: z
    .array(ApiManifestSchema)
    .nullish()
    .transform((value) => value ?? []),
  resourceDiffs: z
    .array(
      resourceDiffSummarySchema
        .nullish()
        .transform((value) => value ?? resourceDiffSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  space: spaceSummarySchema
    .nullish()
    .transform((value) => value ?? spaceSummarySchema.parse({})),
});
export type planData = z.output<typeof planDataSchema>;

export const ApiPostgresBackupSchema = z.object({
  archiveId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  backupId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  completedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  eligible: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  majorVersion: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  reason: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  recoveryEnd: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  recoveryStart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  sizeBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  sourceId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  sourceName: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiPostgresBackup = z.output<typeof ApiPostgresBackupSchema>;

export const ApiPostgresProtectionSchema = z.object({
  status: z.string().default(""),
  message: z.string().default(""),
  lastBackupAt: z.string().nullish(),
  lastTestAt: z.string().nullish(),
  recoveryWindowDays: z.number().default(0),
});
export type ApiPostgresProtection = z.output<
  typeof ApiPostgresProtectionSchema
>;

export const ApiPostgresRestoreCheckSchema = z.object({
  cleanup: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  completedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  durationSeconds: z
    .number()
    .nullish()
    .transform((value) => value ?? 0)
    .nullish()
    .transform((value) => value ?? null),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  operation: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  snapshotStatus: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  startedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  status: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiPostgresRestoreCheck = z.output<
  typeof ApiPostgresRestoreCheckSchema
>;

export const ApiPostgresResourcesSchema = z.object({
  limits: z.record(z.unknown()).optional(),
  requests: z.record(z.unknown()).optional(),
});
export type ApiPostgresResources = z.output<typeof ApiPostgresResourcesSchema>;
export const ApiPostgresRestoreCheckInputSchema = z.object({
  resources: ApiPostgresResourcesSchema.optional(),
  schedule: z.string().optional(),
  timeoutSeconds: z.number().optional(),
});
export type ApiPostgresRestoreCheckInput = z.output<
  typeof ApiPostgresRestoreCheckInputSchema
>;
export const ApiPostgresBackupPolicySchema = z.object({
  enabled: z.boolean().optional(),
  recoveryWindowDays: z.number().optional(),
  schedule: z.string().optional(),
  restoreCheck: ApiPostgresRestoreCheckInputSchema.optional(),
});
export type ApiPostgresBackupPolicy = z.output<
  typeof ApiPostgresBackupPolicySchema
>;
export const ApiPostgresInputSchema = z
  .object({
    backup: ApiPostgresBackupPolicySchema.optional(),
    credentials: z
      .object({
        rotationPeriod: z.string().optional(),
        host: z.string().optional(),
        port: z.number().optional(),
      })
      .optional(),
    database: z.string().optional(),
    username: z.string().optional(),
    image: z.string().optional(),
    instances: z.number().optional(),
    resources: ApiPostgresResourcesSchema.optional(),
    storage: z.string().optional(),
    storageClass: z.string().optional(),
  })
  .passthrough();
export type ApiPostgresInput = z.output<typeof ApiPostgresInputSchema>;

export const ApiPostgresSchema = z.object({
  clusterName: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  environmentGroup: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  operation: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  protection: ApiPostgresProtectionSchema.nullish().transform(
    (value) => value ?? ApiPostgresProtectionSchema.parse({}),
  ),
  restoreCheck: ApiPostgresRestoreCheckSchema.nullish().transform(
    (value) => value ?? ApiPostgresRestoreCheckSchema.parse({}),
  ),
  specification: ApiPostgresInputSchema.nullish().transform(
    (value) => value ?? ApiPostgresInputSchema.parse({}),
  ),
  state: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  updatedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiPostgres = z.output<typeof ApiPostgresSchema>;

export const postgresPageDataSchema = z.object({
  BackupProblem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Backups: z
    .array(
      ApiPostgresBackupSchema.nullish().transform(
        (value) => value ?? ApiPostgresBackupSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  Database: ApiPostgresSchema.nullish().transform(
    (value) => value ?? ApiPostgresSchema.parse({}),
  ),
  Databases: z
    .array(
      ApiPostgresSchema.nullish().transform(
        (value) => value ?? ApiPostgresSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  End: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Operation: ApiOperationSchema.nullish().transform(
    (value) => value ?? ApiOperationSchema.parse({}),
  ),
  Path: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Range: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Search: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Subtab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Tab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type postgresPageData = z.output<typeof postgresPageDataSchema>;

export const ApiComponentOverrideSchema = z.object({
  chart: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  repo: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  values: z
    .record(z.unknown())
    .nullish()
    .transform((value) => value ?? {})
    .nullish()
    .transform((value) => value ?? null),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
});
export type ApiComponentOverride = z.output<typeof ApiComponentOverrideSchema>;

export const ApiComponentConfigurationSchema = z.object({
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  chart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  description: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  override: ApiComponentOverrideSchema.nullish().transform(
    (value) => value ?? ApiComponentOverrideSchema.parse({}),
  ),
  repository: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  values: z
    .record(z.unknown())
    .nullish()
    .transform((value) => value ?? {}),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiComponentConfiguration = z.output<
  typeof ApiComponentConfigurationSchema
>;

export const ApiComponentScopeSchema = z
  .string()
  .nullish()
  .transform((value) => value ?? "");
export type ApiComponentScope = z.output<typeof ApiComponentScopeSchema>;

export const ApiComponentCatalogSchema = z.object({
  clusterId: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  components: z
    .array(
      ApiComponentConfigurationSchema.nullish().transform(
        (value) => value ?? ApiComponentConfigurationSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  profile: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  scope: ApiComponentScopeSchema,
});
export type ApiComponentCatalog = z.output<typeof ApiComponentCatalogSchema>;

export const managementComponentRowSchema = z.object({
  Name: z.string().default(""),
  Version: z.string().default(""),
  URL: z.string().default(""),
  Selected: z.boolean().default(false),
});
export type managementComponentRow = z.output<
  typeof managementComponentRowSchema
>;

export const ApiManagementS3Schema = z.object({
  bucket: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  credentialsRef: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  endpoint: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  pathStyle: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  prefix: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  region: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiManagementS3 = z.output<typeof ApiManagementS3Schema>;

const offsiteSettingsSchema = z.object({
  enabled: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  repository: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  region: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  interval: z
    .string()
    .nullish()
    .transform((value) => value ?? "6h"),
  keepLast: z
    .number()
    .nullish()
    .transform((value) => value ?? 14),
  credentialsRef: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});

export const managementSettingsDataSchema = z.object({
  BackupCoverage: z
    .object({
      checkedAt: z.string(),
      state: z.enum(["available", "attention"]),
      components: z.array(
        z.object({
          kind: z.string(),
          name: z.string(),
          state: z.enum([
            "available",
            "disabled",
            "missing",
            "stale",
            "failed",
            "unknown",
          ]),
          lastSuccess: z.string().nullish(),
          staleAfter: z.string().nullish(),
          message: z.string(),
        }),
      ),
    })
    .nullish(),
  BackupCoverageError: z.string().default(""),
  Offsite: offsiteSettingsSchema
    .nullish()
    .transform((value) => value ?? offsiteSettingsSchema.parse({})),
  Catalog: ApiComponentCatalogSchema.nullish().transform(
    (value) => value ?? ApiComponentCatalogSchema.parse({}),
  ),
  Components: z
    .array(managementComponentRowSchema)
    .nullish()
    .transform((value) => value ?? []),
  Operation: ApiOperationSchema.nullish().transform(
    (value) => value ?? ApiOperationSchema.parse({}),
  ),
  Selected: ApiComponentConfigurationSchema.nullish().transform(
    (value) => value ?? ApiComponentConfigurationSchema.parse({}),
  ),
  Storage: ApiManagementS3Schema.nullish().transform(
    (value) => value ?? ApiManagementS3Schema.parse({}),
  ),
});
export type managementSettingsData = z.output<
  typeof managementSettingsDataSchema
>;

export const infrastructureSelectionSchema = z.object({
  Node: z.enum(["", "management", "application"]).default(""),
  Organization: z.string().default(""),
  Cluster: z.string().default(""),
  Tab: z.string().default(""),
  ComponentScope: z.string().default(""),
  Component: z.string().default(""),
  Operation: z.string().default(""),
});
export type infrastructureSelection = z.output<
  typeof infrastructureSelectionSchema
>;

const organizationOidcSchema = z.object({
  callbackUrls: z.array(z.string()).default([]),
  logoutCallbackUrls: z.array(z.string()).default([]),
  adminEmail: z.string().default(""),
  borqPublicUrl: z.string().default(""),
  issuerUrl: z.string().default(""),
  clientId: z.string().default(""),
  callbackUrl: z.string().default(""),
  logoutCallbackUrl: z.string().default(""),
  clientSecretConfigured: z.boolean().default(false),
  revision: z.number().default(0),
  lastTestedAt: z.string().default(""),
});

const organizationPocketIdSchema = z.object({
  available: z.boolean().default(false),
  state: z.string().default(""),
  message: z.string().default(""),
  issuerUrl: z.string().default(""),
  borqPublicUrl: z.string().default(""),
  certificateIssuer: z.string().default(""),
  adminUsername: z.string().default(""),
  adminEmail: z.string().default(""),
  env: ApiEnvReferenceSchema.nullish(),
});

const spacePocketIdInputSchema = z.object({
  issuerUrl: z.string().default(""),
  adminUsername: z.string().default(""),
  adminEmail: z.string().default(""),
  storage: z.string().default(""),
  storageClass: z.string().default(""),
  image: z.string().default(""),
  certificateIssuer: z.string().default(""),
});
const spacePocketIdSchema = z.object({
  id: z.string().default(""),
  name: z.string().default(""),
  namespace: z.string().default(""),
  state: z.string().default(""),
  message: z.string().default(""),
  operation: z.string().default(""),
  updatedAt: z.string().default(""),
  specification: spacePocketIdInputSchema.default({}),
});
const spacePocketIdPageDataSchema = z.object({
  Services: z
    .array(spacePocketIdSchema)
    .nullish()
    .transform((value) => value ?? []),
  Selected: spacePocketIdSchema
    .nullish()
    .transform((value) => value ?? spacePocketIdSchema.parse({})),
  Operation: ApiOperationSchema.nullish().transform(
    (value) => value ?? ApiOperationSchema.parse({}),
  ),
});

export const pageDataSchema = z.object({
  SpacePocketId: spacePocketIdPageDataSchema
    .nullish()
    .transform((value) => value ?? spacePocketIdPageDataSchema.parse({})),
  SpacePocketIdEnrollment: z.string().default(""),
  ManagementAdmin: z.boolean().default(false),
  CanManageUsers: z.boolean().default(false),
  AwaitingRole: z.boolean().default(false),
  CanConfigureOrganization: z.boolean().default(false),
  Members: z
    .array(
      z.object({
        id: z.string(),
        email: z.string(),
        emailVerified: z.boolean().default(false),
        issuer: z.string(),
        subject: z.string().default(""),
        roleIds: z
          .array(z.string())
          .nullish()
          .transform((value) => value ?? []),
        permissions: z
          .array(z.string())
          .nullish()
          .transform((value) => value ?? []),
        state: z.enum(["active", "disabled"]),
        revision: z.number().int().positive(),
      }),
    )
    .nullish()
    .transform((value) => value ?? []),
  MemberPermissions: z
    .array(z.string())
    .nullish()
    .transform((value) => value ?? []),
  Roles: z
    .array(
      z.object({
        id: z.string(),
        name: z.string(),
        permissions: z.array(z.string()),
        revision: z.number().int().positive(),
      }),
    )
    .nullish()
    .transform((value) => value ?? []),
  PocketId: organizationPocketIdSchema
    .nullish()
    .transform((value) => value ?? organizationPocketIdSchema.parse({})),
  Oidc: organizationOidcSchema
    .nullish()
    .transform((value) => value ?? organizationOidcSchema.parse({})),
  SpaceTab: z
    .string()
    .nullish()
    .transform((value) => value ?? "overview"),
  Registry: z
    .object({ Configured: z.boolean().default(false) })
    .nullish()
    .transform((value) => value ?? { Configured: false }),
  Infrastructure: infrastructureSelectionSchema
    .nullish()
    .transform((value) => value ?? infrastructureSelectionSchema.parse({})),
  AppPath: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Application: ApiAppSchema.nullish().transform(
    (value) => value ?? ApiAppSchema.parse({}),
  ),
  ApplicationCount: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  ApplicationEditor: applicationEditorDataSchema
    .nullish()
    .transform((value) => value ?? applicationEditorDataSchema.parse({})),
  ApplicationManifests: z
    .array(ApiManifestSchema)
    .nullish()
    .transform((value) => value ?? []),
  ApplicationSubtab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ApplicationTab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Applications: applicationListDataSchema
    .nullish()
    .transform((value) => value ?? applicationListDataSchema.parse({})),
  Cluster: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ClusterPath: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ClusterSummaries: z
    .array(
      clusterSummarySchema
        .nullish()
        .transform((value) => value ?? clusterSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Clusters: z
    .array(
      ApiClusterSchema.nullish().transform(
        (value) => value ?? ApiClusterSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  CustomChart: ApiCustomChartSchema.nullish().transform(
    (value) => value ?? ApiCustomChartSchema.parse({}),
  ),
  CustomChartDraft: customChartEditorDataSchema
    .nullish()
    .transform((value) => value ?? customChartEditorDataSchema.parse({})),
  CustomChartSubtab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  CustomChartTab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  CustomCharts: z
    .array(
      ApiCustomChartSchema.nullish().transform(
        (value) => value ?? ApiCustomChartSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  Draft: ApiAppSchema.nullish().transform(
    (value) => value ?? ApiAppSchema.parse({}),
  ),
  EnvironmentEditor: environmentEditorDataSchema
    .nullish()
    .transform((value) => value ?? environmentEditorDataSchema.parse({})),
  Secret: z
    .object({
      name: z.string().default(""),
      type: z.string().default("secret-group"),
      data: z
        .record(z.string(), z.string())
        .nullish()
        .transform((value) => value ?? {}),
      version: z.number().default(0),
      managedBy: z.string().optional(),
    })
    .nullish()
    .transform(
      (value) =>
        value ?? { name: "", type: "secret-group", data: {}, version: 0 },
    ),
  Group: ApiEnvironmentGroupSchema.nullish().transform(
    (value) => value ?? ApiEnvironmentGroupSchema.parse({}),
  ),
  GroupApplications: z
    .array(
      applicationSummarySchema
        .nullish()
        .transform((value) => value ?? applicationSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  GroupLatestVersion: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  GroupTab: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  GroupVersions: z
    .array(
      z
        .number()
        .nullish()
        .transform((value) => value ?? 0),
    )
    .nullish()
    .transform((value) => value ?? []),
  Groups: z
    .array(
      environmentGroupSummarySchema
        .nullish()
        .transform((value) => value ?? environmentGroupSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Location: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Monitoring: monitoringViewDataSchema
    .nullish()
    .transform((value) => value ?? monitoringViewDataSchema.parse({})),
  Notice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Operation: ApiOperationSchema.nullish().transform(
    (value) => value ?? ApiOperationSchema.parse({}),
  ),
  Organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Organizations: z
    .array(
      ApiOrganizationSchema.nullish().transform(
        (value) => value ?? ApiOrganizationSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  Plan: planDataSchema
    .nullish()
    .transform((value) => value ?? planDataSchema.parse({})),
  Postgres: postgresPageDataSchema
    .nullish()
    .transform((value) => value ?? postgresPageDataSchema.parse({})),
  PromptCluster: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  ReviewReturnUrl: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SectionTitle: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SectionView: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  SelectedCluster: clusterSummarySchema
    .nullish()
    .transform((value) => value ?? clusterSummarySchema.parse({})),
  Settings: managementSettingsDataSchema
    .nullish()
    .transform((value) => value ?? managementSettingsDataSchema.parse({})),
  Space: spaceSummarySchema
    .nullish()
    .transform((value) => value ?? spaceSummarySchema.parse({})),
  Spaces: z
    .array(
      spaceSummarySchema
        .nullish()
        .transform((value) => value ?? spaceSummarySchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Title: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  View: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type pageData = z.output<typeof pageDataSchema>;

export const ApiComponentChangeSchema = z.object({
  action: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  chart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  desiredVersion: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  installedVersion: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  reason: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  upgradeAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  valuesDiff: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiComponentChange = z.output<typeof ApiComponentChangeSchema>;

export const ApiComponentDiffSchema = z.object({
  components: z
    .array(
      ApiComponentChangeSchema.nullish().transform(
        (value) => value ?? ApiComponentChangeSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
});
export type ApiComponentDiff = z.output<typeof ApiComponentDiffSchema>;

export const ApiComponentStateSchema = z.object({
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  changed: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  chart: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  status: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  upgradeAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiComponentState = z.output<typeof ApiComponentStateSchema>;

export const ApiPlatformResultSchema = z.object({
  components: z
    .array(
      ApiComponentStateSchema.nullish().transform(
        (value) => value ?? ApiComponentStateSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  profile: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiPlatformResult = z.output<typeof ApiPlatformResultSchema>;

export const ApiComponentInstallationSchema = z.object({
  clusterId: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  diff: ApiComponentDiffSchema.nullish().transform(
    (value) => value ?? ApiComponentDiffSchema.parse({}),
  ),
  id: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  nextRetryAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  result: ApiPlatformResultSchema.nullish().transform(
    (value) => value ?? ApiPlatformResultSchema.parse({}),
  ),
  status: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  updatedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiComponentInstallation = z.output<
  typeof ApiComponentInstallationSchema
>;

export const componentDataSchema = z.object({
  Installation: ApiComponentInstallationSchema.nullish().transform(
    (value) => value ?? ApiComponentInstallationSchema.parse({}),
  ),
  Location: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Problem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Title: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type componentData = z.output<typeof componentDataSchema>;

export const ApiClusterSetupComponentSchema = z.object({
  description: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiClusterSetupComponent = z.output<
  typeof ApiClusterSetupComponentSchema
>;

export const ApiClusterSetupSchema = z.object({
  components: z
    .array(
      ApiClusterSetupComponentSchema.nullish().transform(
        (value) => value ?? ApiClusterSetupComponentSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  namespace: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiClusterSetup = z.output<typeof ApiClusterSetupSchema>;

export const clusterSetupDataSchema = z.object({
  Problem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Setup: ApiClusterSetupSchema.nullish().transform(
    (value) => value ?? ApiClusterSetupSchema.parse({}),
  ),
});
export type clusterSetupData = z.output<typeof clusterSetupDataSchema>;

export const clusterCapacityDataSchema = z.object({
  Available: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  Capacity: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Percent: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Title: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Value: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type clusterCapacityData = z.output<typeof clusterCapacityDataSchema>;

export const clusterNodeDataSchema = z.object({
  Cpu: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  CpuCapacity: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  DiskAllocatable: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  DiskCapacity: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Memory: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  MemoryCapacity: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Pressure: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  State: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Unschedulable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
});
export type clusterNodeData = z.output<typeof clusterNodeDataSchema>;

export const ApiClusterNodeMetricsSchema = z.object({
  cpuAllocatableCores: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  cpuUsageCores: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  conditions: z
    .array(
      z
        .string()
        .nullish()
        .transform((value) => value ?? ""),
    )
    .nullish()
    .transform((value) => value ?? []),
  diskAllocatableBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskCapacityAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  diskCapacityBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskUsageAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  diskUsageBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskUsageCapacityBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskUsageObservedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  memoryAllocatableBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  memoryUsageBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  podCapacity: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  ready: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  unschedulable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  usageAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  usageObservedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiClusterNodeMetrics = z.output<
  typeof ApiClusterNodeMetricsSchema
>;

export const ApiClusterPodMetricsSchema = z.object({
  failed: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  pending: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  restarts: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  running: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  succeeded: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  unknown: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiClusterPodMetrics = z.output<typeof ApiClusterPodMetricsSchema>;

export const ApiClusterMetricsSchema = z.object({
  nodes: z
    .array(
      ApiClusterNodeMetricsSchema.nullish().transform(
        (value) => value ?? ApiClusterNodeMetricsSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  observedAt: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  pods: ApiClusterPodMetricsSchema.nullish().transform(
    (value) => value ?? ApiClusterPodMetricsSchema.parse({}),
  ),
  usageNotice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiClusterMetrics = z.output<typeof ApiClusterMetricsSchema>;

export const clusterMetricsDataSchema = z.object({
  ActivePods: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Cpu: clusterCapacityDataSchema
    .nullish()
    .transform((value) => value ?? clusterCapacityDataSchema.parse({})),
  Disk: clusterCapacityDataSchema
    .nullish()
    .transform((value) => value ?? clusterCapacityDataSchema.parse({})),
  Location: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Memory: clusterCapacityDataSchema
    .nullish()
    .transform((value) => value ?? clusterCapacityDataSchema.parse({})),
  Nodes: z
    .array(
      clusterNodeDataSchema
        .nullish()
        .transform((value) => value ?? clusterNodeDataSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  PodCapacity: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  PressureNodes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Problem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  ReadyNodes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Snapshot: ApiClusterMetricsSchema.nullish().transform(
    (value) => value ?? ApiClusterMetricsSchema.parse({}),
  ),
});
export type clusterMetricsData = z.output<typeof clusterMetricsDataSchema>;

export const usageEventSchema = z.object({
  Clock: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Kind: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Time: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  X: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type usageEvent = z.output<typeof usageEventSchema>;

export const usageSampleSchema = z.object({
  Time: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Value: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  X: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Y: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type usageSample = z.output<typeof usageSampleSchema>;

export const usageChartSchema = z.object({
  Average: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  End: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Events: z
    .array(
      usageEventSchema
        .nullish()
        .transform((value) => value ?? usageEventSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Latest: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Maximum: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Peak: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Samples: z
    .array(
      usageSampleSchema
        .nullish()
        .transform((value) => value ?? usageSampleSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Segments: z
    .array(
      z
        .string()
        .nullish()
        .transform((value) => value ?? ""),
    )
    .nullish()
    .transform((value) => value ?? []),
  Start: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Step: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  Title: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Unit: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type usageChart = z.output<typeof usageChartSchema>;

export const ApiAppLogEntrySchema = z.object({
  container: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  image: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  pod: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  replicaSet: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  timestamp: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiAppLogEntry = z.output<typeof ApiAppLogEntrySchema>;

export const ApiApplicationEventSchema = z.object({
  deployment: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  kind: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  pod: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  revision: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  timestamp: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiApplicationEvent = z.output<typeof ApiApplicationEventSchema>;

export const applicationLogRowSchema = z.object({
  Entry: ApiAppLogEntrySchema.nullish().transform((value) => value ?? null),
  Event: ApiApplicationEventSchema.nullish().transform(
    (value) => value ?? null,
  ),
  OperationPath: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Timestamp: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type applicationLogRow = z.output<typeof applicationLogRowSchema>;

export const ApiChartInputSchema = z.object({
  name: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  repository: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  version: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiChartInput = z.output<typeof ApiChartInputSchema>;

export const ApiChartDefaultsSchema = z.object({
  chart: ApiChartInputSchema.nullish().transform(
    (value) => value ?? ApiChartInputSchema.parse({}),
  ),
  values: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiChartDefaults = z.output<typeof ApiChartDefaultsSchema>;

export const ApiDeploymentSchema = z.object({
  configuration: ApiAppSchema.nullish().transform(
    (value) => value ?? ApiAppSchema.parse({}),
  ),
  operation: ApiOperationSchema.nullish().transform(
    (value) => value ?? ApiOperationSchema.parse({}),
  ),
  revision: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiDeployment = z.output<typeof ApiDeploymentSchema>;

export const ApiApplicationDetailsStateSchema = z
  .string()
  .nullish()
  .transform((value) => value ?? "");
export type ApiApplicationDetailsState = z.output<
  typeof ApiApplicationDetailsStateSchema
>;

export const ApiApplicationDetailsSchema = z.object({
  configuration: ApiAppSchema.nullish().transform(
    (value) => value ?? ApiAppSchema.parse({}),
  ),
  message: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  resources: z
    .array(
      ApiResourceStatusSchema.nullish().transform(
        (value) => value ?? ApiResourceStatusSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  state: ApiApplicationDetailsStateSchema,
});
export type ApiApplicationDetails = z.output<
  typeof ApiApplicationDetailsSchema
>;

export const ApiEventHistorySchema = z.object({
  end: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  events: z
    .array(
      ApiApplicationEventSchema.nullish().transform(
        (value) => value ?? ApiApplicationEventSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  eventsNotice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  start: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiEventHistory = z.output<typeof ApiEventHistorySchema>;

export const ApiAppLogsSchema = z.object({
  end: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  entries: z
    .array(
      ApiAppLogEntrySchema.nullish().transform(
        (value) => value ?? ApiAppLogEntrySchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  events: z
    .array(
      ApiApplicationEventSchema.nullish().transform(
        (value) => value ?? ApiApplicationEventSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  eventsNotice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  limitReached: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  start: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type ApiAppLogs = z.output<typeof ApiAppLogsSchema>;

export const applicationPanelDataSchema = z.object({
  Application: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Cpu: usageChartSchema
    .nullish()
    .transform((value) => value ?? usageChartSchema.parse({})),
  Deployments: z
    .array(
      ApiDeploymentSchema.nullish().transform(
        (value) => value ?? ApiDeploymentSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  Details: ApiApplicationDetailsSchema.nullish().transform(
    (value) => value ?? ApiApplicationDetailsSchema.parse({}),
  ),
  Endpoint: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  EventsNotice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Follow: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  History: ApiEventHistorySchema.nullish().transform(
    (value) => value ?? ApiEventHistorySchema.parse({}),
  ),
  Location: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Logs: ApiAppLogsSchema.nullish().transform(
    (value) => value ?? ApiAppLogsSchema.parse({}),
  ),
  Memory: usageChartSchema
    .nullish()
    .transform((value) => value ?? usageChartSchema.parse({})),
  Organization: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Problem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Range: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Rows: z
    .array(
      applicationLogRowSchema
        .nullish()
        .transform((value) => value ?? applicationLogRowSchema.parse({})),
    )
    .nullish()
    .transform((value) => value ?? []),
  Search: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Space: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type applicationPanelData = z.output<typeof applicationPanelDataSchema>;

export const ApiMetricSampleSchema = z.object({
  timestamp: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  value: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiMetricSample = z.output<typeof ApiMetricSampleSchema>;

export const ApiAppMetricsSchema = z.object({
  cpuCores: z
    .array(
      ApiMetricSampleSchema.nullish().transform(
        (value) => value ?? ApiMetricSampleSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  end: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  events: z
    .array(
      ApiApplicationEventSchema.nullish().transform(
        (value) => value ?? ApiApplicationEventSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  eventsNotice: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  memoryBytes: z
    .array(
      ApiMetricSampleSchema.nullish().transform(
        (value) => value ?? ApiMetricSampleSchema.parse({}),
      ),
    )
    .nullish()
    .transform((value) => value ?? []),
  start: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  stepSeconds: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
});
export type ApiAppMetrics = z.output<typeof ApiAppMetricsSchema>;

export const ApiPostgresMetricsSchema = z.object({
  activeQueries: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  activityAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  connections: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskAvailable: z
    .boolean()
    .nullish()
    .transform((value) => value ?? false),
  diskAvailableBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskCapacityBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  diskUsedBytes: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  notice: z
    .string()
    .nullish()
    .transform((value) => value ?? "")
    .nullish()
    .transform((value) => value ?? null),
  protection: ApiPostgresProtectionSchema.nullish().transform(
    (value) => value ?? ApiPostgresProtectionSchema.parse({}),
  ),
  transactions: z
    .number()
    .nullish()
    .transform((value) => value ?? 0),
  usage: ApiAppMetricsSchema.nullish().transform(
    (value) => value ?? ApiAppMetricsSchema.parse({}),
  ),
});
export type ApiPostgresMetrics = z.output<typeof ApiPostgresMetricsSchema>;

export const postgresPanelDataSchema = z.object({
  Cpu: usageChartSchema
    .nullish()
    .transform((value) => value ?? usageChartSchema.parse({})),
  Location: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
  Logs: ApiAppLogsSchema.nullish().transform(
    (value) => value ?? ApiAppLogsSchema.parse({}),
  ),
  Memory: usageChartSchema
    .nullish()
    .transform((value) => value ?? usageChartSchema.parse({})),
  Metrics: ApiPostgresMetricsSchema.nullish().transform(
    (value) => value ?? ApiPostgresMetricsSchema.parse({}),
  ),
  Problem: z
    .string()
    .nullish()
    .transform((value) => value ?? ""),
});
export type postgresPanelData = z.output<typeof postgresPanelDataSchema>;
