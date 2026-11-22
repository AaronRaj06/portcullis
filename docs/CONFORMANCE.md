# Gateway API conformance

Portcullis v0.3.0 was run against the upstream Gateway API conformance suite, **v1.7.0, standard channel**, profile `GATEWAY-HTTP`, with the supported features `Gateway`, `HTTPRoute` and `ReferenceGrant` (core only). The suite ran unmodified, in a Pod inside the `make demo` kind cluster (`make conformance`, `deploy/conformance.sh`), against commit `3653c11`. v0.3.0 adds only documentation on top of that commit.

## Result

| | Count |
|---|---|
| Pass | **40** |
| Fail | **0** |
| Skip (extended features not claimed) | 130 |

The suite's own report reads **"GATEWAY-HTTP: Core tests succeeded."** It is self-run and has not been submitted upstream, so this is not a listed conformance claim. Extended features (redirect scheme, port and path; URL rewrites; mirrors; response header modifiers; regex and query matches; ports other than 80 and 443, and more) are neither claimed nor tested.

Evidence, in `bench/results/conformance/`:

- `report.yaml`: the suite's ConformanceReport.
- `results.txt`: one line per test.
- `conformance.log.gz`: the full log.

## What it took (v0.2.1 to v0.3.0)

| Change | Tests that now pass |
|---|---|
| HTTPS listeners: certificates from Secrets (with ReferenceGrant), SNI, live rotation | GatewayInvalidTLSConfiguration, GatewaySecretInvalidReferenceGrant, GatewaySecretMissingReferenceGrant, GatewaySecretReferenceGrantAllInNamespace, GatewaySecretReferenceGrantSpecific, GatewayModifyListeners, GatewayWithAttachedRoutes, HTTPRouteHTTPSListener |
| RequestHeaderModifier and RequestRedirect filters | HTTPRouteRequestHeaderModifier, HTTPRouteRedirectHostAndStatus |
| One Service and address per Gateway | HTTPRouteMultipleGateways |

The design is in [ADR 0013](adr/0013-listeners-tls-and-tenant-isolation.md).

The conformance runner also needed two workarounds before v0.3.0, both now removed. It deleted the demo app during a run, because Gateways shared an address. It also patched the suite's HTTPS Gateway out of readiness checks, because HTTPS was not supported.

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
| 4 (v0.2.1) | 29 | 11 | Two earlier attempts failed on an overloaded host and are not counted. |
| 5 (v0.3.0) | **40** | **0** | This page. The suite ran unmodified, for the first time. |

What remains, with no work planned, is listed in [ROADMAP.md](ROADMAP.md) under "Deferred from Phase 6".
