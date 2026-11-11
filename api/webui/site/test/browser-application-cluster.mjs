// Read-only discovery and absent resources let browser tests review a first
// deployment. This fixture does not admit writes or simulate controller results.
export function serveApplicationCluster(request, response) {
  if (request.method !== "GET") return false;
  const path = new URL(request.url, "http://cluster").pathname;
  const groups = [
    {
      group: "apps",
      version: "v1",
      resources: [["deployments", "Deployment"]],
    },
    {
      group: "gateway.networking.k8s.io",
      version: "v1",
      resources: [
        ["gateways", "Gateway"],
        ["httproutes", "HTTPRoute"],
      ],
    },
    {
      group: "cert-manager.io",
      version: "v1",
      resources: [
        ["certificates", "Certificate"],
        ["clusterissuers", "ClusterIssuer"],
      ],
    },
    {
      group: "external-secrets.io",
      version: "v1",
      resources: [
        ["externalsecrets", "ExternalSecret"],
        ["clustersecretstores", "ClusterSecretStore"],
      ],
    },
  ];
  const versions = (entry) => ({
    groupVersion: entry.group + "/" + entry.version,
    version: entry.version,
  });
  const resources = (version, entries) => ({
    kind: "APIResourceList",
    apiVersion: "v1",
    groupVersion: version,
    resources: entries.map(([name, kind]) => ({
      name,
      kind,
      namespaced: ![
        "Namespace",
        "ClusterIssuer",
        "ClusterSecretStore",
      ].includes(kind),
      verbs: ["get", "list"],
    })),
  });
  let result;
  if (path === "/version") result = { gitVersion: "v1.35.3" };
  else if (path === "/api")
    result = {
      kind: "APIVersions",
      apiVersion: "v1",
      versions: ["v1"],
      serverAddressByClientCIDRs: [],
    };
  else if (path === "/api/v1")
    result = resources("v1", [
      ["services", "Service"],
      ["namespaces", "Namespace"],
      ["secrets", "Secret"],
    ]);
  else if (path === "/apis")
    result = {
      kind: "APIGroupList",
      apiVersion: "v1",
      groups: groups.map((entry) => ({
        name: entry.group,
        versions: [versions(entry)],
        preferredVersion: versions(entry),
      })),
    };
  else if (path === "/apis/cert-manager.io/v1/clusterissuers/borq")
    result = {
      apiVersion: "cert-manager.io/v1",
      kind: "ClusterIssuer",
      metadata: { name: "borq" },
      spec: {
        acme: {
          solvers: [
            {
              selector: { dnsZones: ["borq.io", "example.com"] },
              http01: { ingress: { class: "test" } },
            },
          ],
        },
      },
    };
  else {
    const group = groups.find(
      (entry) => path === "/apis/" + entry.group + "/" + entry.version,
    );
    if (group)
      result = resources(group.group + "/" + group.version, group.resources);
    else if (path.startsWith("/apis/") || path.startsWith("/api/v1/")) {
      response.statusCode = 404;
      result = {
        kind: "Status",
        apiVersion: "v1",
        status: "Failure",
        reason: "NotFound",
        code: 404,
        message: "Resource is absent in the browser fixture.",
      };
    } else return false;
  }
  response.end(JSON.stringify(result));
  return true;
}
