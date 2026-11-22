# ADR 0012: Control-plane design

- Status: accepted
- Date: 2026-11-21

## Context

Phase 6 (brief section 4): a controller that reconciles GatewayClass, Gateway and HTTPRoute; resolves Services through EndpointSlices; pushes versioned, hashed snapshots to the data planes with acknowledgement and retries; writes status conditions; and adds a `PortcullisPolicy` CRD validated with the data plane's own parser.

## Decision

- **One work-queue key, a full rebuild per reconcile.** Every watched kind maps to a single key. Each reconcile lists everything, translates it in a pure function (`Translate`), pushes one snapshot and writes statuses. Changes that arrive together collapse into one reconcile, and every data plane always gets one consistent snapshot. The cost is O(cluster) per event, which is fine to thousands of routes (recorded as a `shortcut:` in `reconcile.go`).
- **A pure translation layer.** `Translate(Inputs) *Output` has no API calls, so the Gateway API semantics (hostname intersection, allowedRoutes, rejections, splits) are unit-tested on plain structs.
- **The snapshot is versioned by its own hash.** `version` is `sha256:<prefix>` of the config, so re-pushing an unchanged desired state is a no-op on the data plane (`changed: false`). The push is acknowledged only when each data plane returns the same hash the controller computed with the same `config` package. A failure returns an error, and the controller-runtime work queue retries with exponential back-off.
- **Data-plane discovery** goes through a headless `portcullis-admin` Service with `publishNotReadyAddresses: true`. A new pod is unready exactly until it gets its first config (`-wait-for-config`), so readiness-filtered endpoints would never include it. `instances` is set to the number of data-plane pods, so rate limits are shared (ADR 0011).
- **Unsupported features are rejected, not approximated:**
  - HTTPS listeners: `UnsupportedProtocol`.
  - Filters, regex matches and query-parameter matches: the rule is dropped and the route marked `PartiallyInvalid`.
  - Cross-namespace backends without a ReferenceGrant: `RefNotPermitted`.
  - Route kinds other than HTTPRoute in `allowedRoutes.kinds`: `InvalidRouteKinds`.

  Serving a rule without its header filter or regex would be silently wrong.
- **Requests that cannot be served are answered by generated PolicyScript.**
  - A route whose backends are all invalid gets `deny(500)`, as Gateway API asks.
  - A route whose Services have no ready endpoints gets `deny(503)`.
  - With no routes at all, a catch-all `deny(404)` is used.

  These all point at a sentinel pool that is never dialled. This keeps the data plane's config schema strict ("every pool has a backend") with no special cases.
- **Each `PortcullisPolicy` is validated on its own** with `policy.Compile`, the data plane's parser. A bad script gets `Accepted=False, reason Invalid` with `line:col` in the message and is left out, so it never blocks other policies. Gateway-targeted policies run before HTTPRoute-targeted ones.
- **Status writes only when something changed.** Conditions are merged so `lastTransitionTime` survives, and other controllers' `parents[]` entries on an HTTPRoute are preserved. Watches on our own kinds use `GenerationChangedPredicate`, so our status updates do not re-trigger reconciles.
- **`Programmed` on HTTPRoute parents** is an addition (core Gateway API has Accepted, ResolvedRefs and PartiallyInvalid on routes). It reports the data-plane acknowledgement, which the brief asks for.
- **Least privilege:**
  - The controller can read Gateway API objects (ReferenceGrants included), policies, Namespaces, Services and EndpointSlices; update only `*/status`; and manage Leases and Events in its own namespace.
  - The data plane has no API access at all (`automountServiceAccountToken: false`).
  - A NetworkPolicy limits the admin port to the controller.
  - The admin token is a Secret generated at install time.
- **A hand-written CRD and deep copies.** Three small structs did not justify adding controller-gen.

## Consequences

- At large scale (tens of thousands of routes), the full rebuild and the config size become the limit. The upgrade path is per-Gateway keys and incremental pushes.
- Since v0.2.1 the controller also watches Namespaces (for `allowedRoutes` selectors) and ReferenceGrants, so the ReferenceGrant CRD must be installed.
- TLS termination at the Gateway (HTTPS listeners) is not done yet; the data plane already supports TLS, but the controller does not program certificates.
