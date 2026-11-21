# Gateway API conformance

Portcullis was run against the upstream Gateway API conformance suite, **v1.7.0, standard channel**, profile `GATEWAY-HTTP`, with the supported features `Gateway`, `HTTPRoute` and `ReferenceGrant` (core only). The suite ran in a Pod inside the `make demo` kind cluster (`make conformance`, `deploy/conformance.sh`).

## Result

| | Count |
|---|---|
| Pass | **22** |
| Fail | **18** |
| Skip (extended features not claimed) | 130 |

This is a failing core profile: Portcullis is **not conformant**. The table below gives the cause of every failure. Each failure was also rerun on its own (`RUN_TEST=<name> make conformance`) on a quiet host, and every one failed again with the same cause.

Raw output: `bench/results/conformance/results.txt` (the full run), and `bench/results/conformance/<Test>/` (each rerun; logs gzipped). The full run's `conformance.log` holds only the end of the run, because the kubelet rotated the container log.

## Passing (22)

GatewayClassObservedGenerationBump, GatewayListenerUnsupportedProtocol, GatewayNameMaximumLength, GatewayObservedGenerationBump, GatewayPreserveForeignConditions, HTTPRouteExactPathMatching, HTTPRouteHeaderMatching, HTTPRouteHostnameIntersection, HTTPRouteInvalidBackendRefUnknownKind, HTTPRouteInvalidCrossNamespaceBackendRef, HTTPRouteInvalidNonExistentBackendRef, HTTPRouteInvalidParentRefNotMatchingSectionName, HTTPRouteInvalidReferenceGrant, HTTPRouteMatching, HTTPRouteMatchingAcrossRoutes, HTTPRouteObservedGenerationBump, HTTPRoutePathMatchOrder, HTTPRoutePreserveForeignStatus, HTTPRouteRuleOrder, HTTPRouteServiceTypes, HTTPRouteSimpleSameNamespace, HTTPRouteWeight.

## Failing (18), by cause

| Cause | Tests | Kind |
|---|---|---|
| **HTTPS listeners not implemented.** Non-HTTP listeners are rejected with `UnsupportedProtocol`, so certificate and Secret checks never run, and a Gateway with only HTTPS listeners is not accepted. | GatewayInvalidTLSConfiguration, GatewaySecretInvalidReferenceGrant, GatewaySecretMissingReferenceGrant, GatewaySecretReferenceGrantAllInNamespace, GatewaySecretReferenceGrantSpecific, HTTPRouteHTTPSListener, GatewayModifyListeners | Missing feature |
| **ReferenceGrant not implemented.** Cross-namespace `backendRefs` are always refused with `RefNotPermitted`, even when a grant allows them. | HTTPRouteReferenceGrant, HTTPRoutePartiallyInvalidViaInvalidReferenceGrant | Missing feature |
| **HTTPRoute filters not implemented.** Rules with filters are refused with `UnsupportedValue`. | HTTPRouteRequestHeaderModifier, HTTPRouteRedirectHostAndStatus | Missing feature |
| **`allowedRoutes.namespaces.from: Selector` is treated as not allowed.** Routes from labelled namespaces get `NotAllowedByListeners`. | HTTPRouteCrossNamespace, HTTPRouteInvalidCrossNamespaceParentRef¹ | Missing feature |
| **One shared data-plane address.** Every Gateway is served on the same address and port, so routes on different Gateways collide (first match wins). | HTTPRouteMultipleGateways, HTTPRouteListenerHostnameMatching | Design limit |
| **`allowedRoutes.kinds` is not validated.** A listener that allows only an unsupported kind should report `ResolvedRefs=False, InvalidRouteKinds`; Portcullis reports `True`. | GatewayInvalidRouteKind | Bug |
| **`AttachedRoutes` reads 0** where the suite expects 1 and 2. | GatewayWithAttachedRoutes | Bug, not yet diagnosed |
| **No-backend route status.** Never finished an isolated rerun: the kind API server timed out during setup and cleanup, so the cause is unconfirmed. | HTTPRouteNoBackendRefs | Unconfirmed |

¹ This test also expects a `ResolvedRefs` condition on a route that is not accepted. Portcullis sets only `Accepted=False` in that case.

## Fixed by this run

- **GatewayListenerUnsupportedProtocol** (failed before the fix). A Gateway with some valid and some invalid listeners must stay `Accepted=True` with reason `ListenersNotValid`; Portcullis reported reason `Accepted`. Fixed in `internal/controller/translate.go`, covered by `TestListenerHostnamesAndClasses`.

## History

| Run | Pass | Fail | Notes |
|---|---|---|---|
| 1 | 11 | 29 | The demo app's catch-all route answered the suite's requests. The runner now removes it for the run. |
| 2 | 21 | 19 | Host load average about 47. |
| 3 (this table) | 22 | 18 | Quiet host, with the `ListenersNotValid` fix. |

Planned fixes are tracked in [ROADMAP.md](ROADMAP.md) under "Deferred from Phase 6".
