# Configure service publishing presets

The `spec.publishing` API describes a complete hosted control plane service topology. It is an opt-in alternative to `spec.services` during coexistence, not a change to the default CLI output. Existing HostedClusters continue to use `spec.services` without being rewritten.

## Enable developer preview

`ServicePublishingAPI` is a HyperShift-owned management-cluster feature gate in `DevPreviewNoUpgrade`. Install the corresponding CRD variants and configure the operator feature set together:

```bash
hypershift install --feature-set=DevPreviewNoUpgrade
```

Preview feature sets do not support upgrades. Use a suitable test environment. This setting is independent of `spec.configuration.featureGate.featureSet`, which controls OpenShift features inside a hosted cluster.

`--tech-preview-no-upgrade` remains supported and is equivalent to `--feature-set=TechPreviewNoUpgrade`. It does not enable `ServicePublishingAPI`; do not combine it with developer preview. An operator command-line `--feature-gates` override takes precedence over feature-set defaults. For example, GCP requires the HyperShift Operator's `--feature-gates=GCPPlatform=true` when that gate is not enabled by the selected set; developer-preview CRD availability does not by itself enable the GCP controller gate.

## Choose a topology

| Preset | Service topology | Router exposure |
|--------|------------------|-----------------|
| `SingleIngressPoint` | APIServer, OAuthServer, Konnectivity, and Ignition use Routes through one ingress point, except an allowed OAuth override | `LoadBalancer`, `NodePort`, or `PlatformProvided` |
| `DedicatedAPIServerEndpoint` | APIServer has a dedicated LoadBalancer; other services use management-cluster ingress or their existing private Route path, except an allowed OAuth override | No user-facing HCP router; private connectivity can retain an internal router |
| `NodePort` | Each service uses a separate NodePort with one shared reachable node address | No HCP router |

Endpoint access and private connectivity remain platform settings. Choosing a publishing preset does not change AWS PrivateLink, Azure private endpoints, or GCP Private Service Connect.

### Single ingress point

The APIServer hostname is required because it cannot be derived reliably. The CLI can construct it from `--external-dns-domain`, or you can specify it explicitly. Other service hostnames can be omitted for controller-derived names, or overridden with repeated `--publishing-service-hostname` flags. A selected LoadBalancer arm must be present even when there is no router hostname:

```yaml
spec:
  publishing:
    type: SingleIngressPoint
    singleIngressPoint:
      exposure: LoadBalancer
      loadBalancer: {}
      services:
      - name: APIServer
        hostname: api-example.example.com
      - name: OAuthServer
        hostname: oauth-example.example.com
```

For example, add the following flags to an otherwise complete AWS creation command:

```bash
hypershift create cluster aws \
  --name=example \
  --pull-secret=pull-secret.json \
  --endpoint-access=Private \
  --external-dns-domain=example.com \
  --publishing-type=SingleIngressPoint \
  --publishing-exposure=LoadBalancer
```

For a bare-metal router reachable through a node port, add the following flags to your Agent creation command:

```bash
--publishing-type=SingleIngressPoint \
--publishing-exposure=NodePort \
--publishing-address=192.0.2.10 \
--publishing-port=30443 \
--publishing-service-hostname=APIServer=api-example.example.com
```

This exposes the **router's HTTPS listener**, not four service NodePorts. Services still use Routes and SNI hostnames. Configure DNS and network access so clients can reach the router address and port. Omitting `--publishing-port` allows Kubernetes to allocate it. Router NodePort exposure is supported on Agent, KubeVirt, and None. Agent also supports LoadBalancer and PlatformProvided ingress exposure; it is not limited to NodePort.

`PlatformProvided` uses platform-owned routing instead of creating a new per-HCP load balancer. IBMCloud Route-based publishing uses this mode. ARO HCP retains its shared-ingress path; the CLI defaults to PlatformProvided for managed Azure. Platform routing infrastructure must already exist for this exposure mode.

### Dedicated API server endpoint

All overrides are optional. A policy without its optional arm is valid:

```yaml
spec:
  publishing:
    type: DedicatedAPIServerEndpoint
```

Use `--publishing-type=DedicatedAPIServerEndpoint` to opt in. Only APIServer and OAuthServer hostname overrides are supported by this preset. Konnectivity and Ignition use controller-derived Route hostnames, including internal names where private connectivity requires them.

Azure self-managed clusters may retain OAuth on a separate LoadBalancer by passing the existing `--oauth-publishing-strategy=LoadBalancer` flag. The publishing policy encodes this as `exposure: DedicatedLoadBalancer` on the OAuthServer entry. This also works with SingleIngressPoint, including Azure private endpoint access. It is not an ARO HCP override.

The existing GCP private configuration without ExternalDNS remains supported: APIServer can use a LoadBalancer without an explicit hostname. It is not forced into SingleIngressPoint. Existing private Routes with no external hostname continue to use their internal Route path. No blanket prohibition is added on viable private Ignition or Konnectivity hostname overrides.

### Direct service NodePorts

```yaml
spec:
  publishing:
    type: NodePort
    nodePort:
      address: 192.0.2.10
      services:
      - name: APIServer
        port: 30443
```

Use `--publishing-type=NodePort --publishing-address=192.0.2.10`, optionally repeating `--publishing-service-port=APIServer=30443` for individual services. Ports omitted from the policy are allocated dynamically. Explicit ports must be distinct and between 30000 and 32767. Agent and KubeVirt can also obtain the address from `--api-server-address`; None uses `--external-api-server-address`.

## Coexistence and version skew

- Omit `--publishing-type` to retain the existing `spec.services` CLI output. Other publishing flags require this explicit opt-in.
- HostedCluster accepts exactly one source: non-empty `spec.services` or `spec.publishing`.
- With the gate enabled, the HyperShift Operator translates representable legacy services at the HostedControlPlane boundary and writes both fields. It does not mutate the HostedCluster spec.
- HostedControlPlane allows both fields for compatibility. Existing services remain authoritative when both are present; a translation mismatch is diagnosed rather than changing the legacy topology.
- Older Control Plane Operators can consume the back-translated services for supported topologies. **An older CPO without router NodePort capability causes a translation/configuration error for SingleIngressPoint NodePort. There is no silent LoadBalancer fallback.**
- Agent `SingleIngressPoint` with `PlatformProvided` also requires a publishing-aware CPO, identified by the existing `io.openshift.hypershift.control-plane-operator-supports-router-nodeport=true` image label. An older CPO would turn its API Route into an HCP-owned Route and require an unrequested per-HCP LoadBalancer, so the operator rejects that unsafe translation. A capable CPO supports all three Agent ingress exposures. IBM Cloud and ARO retain their existing PlatformProvided paths.
- An unrepresentable legacy configuration stays on services-only reconciliation. Its translation warning is visible on `ValidConfiguration` without blocking the existing cluster.

## Migrate an existing HostedCluster atomically

`spec.publishing` is immutable as a whole once set, and cannot be removed. This includes its hostnames, exposure, address, and ports. Initial setting during migration is allowed. These HostedCluster immutability rules do not apply to the translated HostedControlPlane policy.

Migration must **remove `spec.services` and set `spec.publishing` in the same API update**. Do not first clear services or add publishing while keeping services. Both intermediate states are rejected. Confirm the operator feature gate, target CPO capabilities, DNS, and network connectivity before changing the topology.

For an existing direct-NodePort cluster, save this merge patch as `publishing-migration.yaml`, substituting the current address and any explicit service ports from its legacy spec:

```yaml
spec:
  services: null
  publishing:
    type: NodePort
    nodePort:
      address: 192.0.2.10
      services:
      - name: APIServer
        port: 30443
      - name: OAuthServer
        port: 30444
      - name: Konnectivity
        port: 30445
      - name: Ignition
        port: 30446
```

Validate the single atomic request before applying it:

```bash
oc patch hostedcluster example -n clusters --type=merge \
  --patch-file=publishing-migration.yaml --dry-run=server -o yaml
oc patch hostedcluster example -n clusters --type=merge \
  --patch-file=publishing-migration.yaml
```

For a Route-based cluster, use the matching SingleIngressPoint policy instead, preserving hostnames and any Azure OAuth dedicated LoadBalancer override. Adopting router NodePort is also possible through this one-time migration, but changing exposure is an intentional topology change and requires connectivity planning. Review the full desired policy carefully before setting an immutable field.

This implementation does not reject or remove legacy services, and does not migrate ROSA, OCM, ACM, ARO-HCP, or other external tooling.

## Inspect configuration and diagnostics

```bash
oc get hostedcluster example -n clusters \
  -o jsonpath='{.status.conditions[?(@.type=="ValidConfiguration")]}'
oc get hostedcluster example -n clusters -o jsonpath='{.spec.publishing}'
oc get hostedcontrolplane example -n clusters-example -o yaml
```

Inspect `ServicePublishingTranslationWarning` for legacy fallback diagnostics, and configuration errors for unsupported target-CPO capabilities. Disabling the gate stops new legacy-to-publishing translation; existing publishing policies continue to be read. Keep the appropriate CRD schema installed for resources already using the field.

See the [legacy service strategy reference](../../reference/service-publishing-strategies.md) for existing platform behavior and the [API reference](../../reference/api.md) for field validation.
