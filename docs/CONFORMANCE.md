# Gateway API conformance

Portcullis v0.2.1 was run against the upstream Gateway API conformance suite, **v1.7.0, standard channel**, profile `GATEWAY-HTTP`, with the supported features `Gateway`, `HTTPRoute` and `ReferenceGrant` (core only). The suite ran in a Pod inside the `make demo` kind cluster (`make conformance`, `deploy/conformance.sh`).

## Result

| | Count |
|---|---|
| Pass | **29** |
| Fail | **11** |
| Skip (extended features not claimed) | 130 |

This is a failing core profile: Portcullis is **not conformant**.

Evidence, in `bench/results/conformance/`:

- `results.txt` and `conformance.log.gz`: the full v0.2.1 run. The causes below are read from this log and from the upstream test manifests.
- `HTTPRouteRedirectHostAndStatus/` and `HTTPRouteMultipleGateways/`: isolated v0.2.1 reruns of those two tests. An isolated rerun of `GatewayWithAttachedRoutes` was attempted and produced no result. The other failures were not rerun on v0.2.1.
- All other per-test directories are reruns from **v0.2.0** and do not describe v0.2.1.

## Failing (11), by cause

| Cause | Tests | Kind |
|---|---|---|
| **HTTPS listeners not implemented.** They are rejected with `UnsupportedProtocol`, so certificate and Secret checks never run, and a Gateway with only HTTPS listeners is not accepted. In `GatewayWithAttachedRoutes` the two HTTP subtests pass and only the HTTPS one fails. | GatewayInvalidTLSConfiguration, GatewaySecretInvalidReferenceGrant, GatewaySecretMissingReferenceGrant, GatewaySecretReferenceGrantAllInNamespace, GatewaySecretReferenceGrantSpecific, GatewayModifyListeners, GatewayWithAttachedRoutes, HTTPRouteHTTPSListener | Missing feature |
| **HTTPRoute filters not implemented.** Rules with filters are refused with `UnsupportedValue`. | HTTPRouteRequestHeaderModifier, HTTPRouteRedirectHostAndStatus | Missing feature |
| **One shared data-plane address.** Routes on different Gateways collide (first match wins), so one Gateway's dedicated route is never reached. | HTTPRouteMultipleGateways | Design limit |

## Passing (29)

GatewayClassObservedGenerationBump, GatewayInvalidRouteKind, GatewayListenerUnsupportedProtocol, GatewayNameMaximumLength, GatewayObservedGenerationBump, GatewayPreserveForeignConditions, HTTPRouteCrossNamespace, HTTPRouteExactPathMatching, HTTPRouteHeaderMatching, HTTPRouteHostnameIntersection, HTTPRouteInvalidBackendRefUnknownKind, HTTPRouteInvalidCrossNamespaceBackendRef, HTTPRouteInvalidCrossNamespaceParentRef, HTTPRouteInvalidNonExistentBackendRef, HTTPRouteInvalidParentRefNotMatchingSectionName, HTTPRouteInvalidReferenceGrant, HTTPRouteListenerHostnameMatching, HTTPRouteMatching, HTTPRouteMatchingAcrossRoutes, HTTPRouteNoBackendRefs, HTTPRouteObservedGenerationBump, HTTPRoutePartiallyInvalidViaInvalidReferenceGrant, HTTPRoutePathMatchOrder, HTTPRoutePreserveForeignStatus, HTTPRouteReferenceGrant, HTTPRouteRuleOrder, HTTPRouteServiceTypes, HTTPRouteSimpleSameNamespace, HTTPRouteWeight.

## Fixed in v0.2.1

| Change | Tests that now pass |
|---|---|
| ReferenceGrant for cross-namespace backends | HTTPRouteReferenceGrant, HTTPRoutePartiallyInvalidViaInvalidReferenceGrant |
| `allowedRoutes` namespace selectors | HTTPRouteCrossNamespace |
| `allowedRoutes.kinds` validation | GatewayInvalidRouteKind |
| `ResolvedRefs` on routes that are not accepted | HTTPRouteInvalidCrossNamespaceParentRef |
| 500 for a rule without backendRefs | HTTPRouteNoBackendRefs |
| Two parentRefs to one Gateway no longer invalidate the snapshot for every Gateway | HTTPRouteListenerHostnameMatching |

## Correction to the v0.2.0 report

The v0.2.0 version of this page and its commit message said that all 18 failures were confirmed by isolated reruns on a quiet host. That was wrong:

- 15 were reproduced. `HTTPRouteListenerHostnameMatching` and `HTTPRouteRedirectHostAndStatus` were never rerun, and the rerun of `HTTPRouteNoBackendRefs` produced no result.
- `HTTPRouteListenerHostnameMatching` was blamed on the shared address. The cause was a serious bug: duplicate route names made the snapshot invalid, which stopped config updates for every Gateway.
- `AttachedRoutes` reading 0 was listed as a separate bug. It followed from the missing namespace selectors.
- The page cited a `conformance.log` that was not committed.

## History

| Run | Pass | Fail | Notes |
|---|---|---|---|
| 1 | 11 | 29 | The demo app's catch-all route answered the suite's requests. The runner now removes it for the run. |
| 2 | 21 | 19 | Host load average about 47. |
| 3 (v0.2.0) | 22 | 18 | With the `ListenersNotValid` fix. |
| 4 (v0.2.1) | 29 | 11 | This page. Two earlier attempts failed on an overloaded host and are not counted. |

What remains, with no work planned, is listed in [ROADMAP.md](ROADMAP.md) under "Deferred from Phase 6".
