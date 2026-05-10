borq is prod when reflected

borq is a all in one cli tool which creates and manages deployment clouds in an opinionated way using kubernetes CRDs as yamls

v1 only deploys clusters using cluster api hetzner

the cluster will install traefik and tailscale by default, and require cloudflare as a dns provider to use their load balancer and tunnel features without needing to pay for a hetzner load balancer

the installed and included applications are as follows:

management cluster:
- tailscale
- cloudnative postgres operator
- prometheus
- loki
- grafana
- cluster api hetzner (capih)
- openbao
- external secrets operator (all secrets stored in openbao first, only openbao unlock keys need stored elsewhere securely)

workload cluster:
- traefik
- cloudflared
- tailscale
- external secrets operator
- grafana alloy (forwards to management cluster by default)
- prometheus (optional)
- loki (optional)


borq itself is a server with a cli. All commands are offered as restful commands with a cli which calls those restful commands. If the current kubecontext does not have a borq server running, then borq management bootstrap must be run

`borq management bootstrap` bootstraps a management cluster. The first slice creates the local management kind cluster and proves it is alive before the borq server is added later.

If the cluster was not made with cluster api hetzner, it should go ahead and install the required workload cluster items anyway. Keep them all in the borq-system namespace unless directed otherwise. We should just need a storage class name if its not capih as capih manages the CSI for the storage classes


- create kind cluster (namespaces per tenant .i.e cloud provider, tailscale operator)
- install openbao (ensure the unlock is stored correctly)
- add hetzner secrets to openbao (cloud-provider)
- add tailscale secrets to openbao (tenant)
- install external secrets
- install tailscale operator
- install cluster api hetzner with ESO secrets
- install prometheus, loki, grafana

App install flows:
- simple yaml for declaring the app layout, without exposing kubernetes words
- no env vars or secrets injected, all are references to openbao paths. 
- secrets/config need a separate creation flow prior to deploying
- builds need to happen prior to deploying


```yaml
application:
  name: "my-strapi-app"
  project: "marketing-site"
  publicly_accessible: true
  storage:
    - name: data
      sizeGb: 5
      mount_point: /app
databases:
  - name: main-psql
```

borq should have a gitops style deployment without needing or relying on git. By default, if someone ran `borq deploy` secret was not present, we should fail out at that time. If a build was not present, we should build in place (for now, support external builders later) and push to the image registry then deploy that same image.
This effectively means that we turn the given app yaml into kubernetes manifests, check whats running currently vs the kubernetes manifests, then perform actions based on whats different. 
This means we should have a `borq plan` which effectively returns that diff


commands:
`borq deploy` - builds and deploys images
`borq secret XXX` - crud for secret management
`borq management bootstrap` - bootstrap the management cluster
`borq management server start` - operates the server which runs in the management cluster
`borq management server reconcile` - reconciles the differences in the management cluster's expected state, and its actual state. This would be used for upgrading components like openbao, keycloak, etc. If a `--dry-run` is passed in, it would not make any changes and instead output the diff for the cluster components that need updated, in the order they would be run.
`borq management XXX` - operations updating the management cluster


Things to look into:
- openbao root token autosetting in prod
- openbao unseal key setting in prod/storing
- openbao autounseal somehow? -> could we set borq up as an autounsealer, and have the CLI send in the data if needed? can we seal/rewrap the openbao with our own code?
- openbao policies and ACLs. Definitely per tenant later. We should probably have one per namespace/app so apps cant take eachothers secrets? helm authDelegator?

openbao bootstrap:
- set root token, tell user to store that and unseal token somewhere else
- autocreate kubernetes auth method, make sure borq server can access it
- priority class name must be set to ensure openbao always schedules
- borq server:
    - upon install, reads root token from secrets, uses it to create kubernetes secrets engine
    - create service account in openbao which links to openbao service account
    - recreate openbao client using kubernetes service account
    - this can all be skipped in helm chart allows default engine enablement

backups:
- openbao
- can caph import/adopt a cluster if its lost?
