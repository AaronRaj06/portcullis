package controller

// Listeners: each Gateway gets its own Service, and each of its ports its own
// target port on the shared data plane, so no Gateway answers on another's address.

import (
	"cmp"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
)

const (
	managedByLabel    = "portcullis.dev/managed-by"
	managedByValue    = "portcullis-controller"
	gatewayAnnotation = "portcullis.dev/gateway"

	// Target ports for Gateway listeners (deploy/manifests NetworkPolicy has the same range).
	firstTargetPort = 10000
	lastTargetPort  = 19999

	// maxCertBytes bounds one certificate and key in the pushed snapshot.
	maxCertBytes = 32 << 10
)

// GatewayPort is one port of a Gateway's Service.
type GatewayPort struct {
	Name   string // protocol-port: a protocol change gets a new target port
	Port   int32
	Target int32
}

// listener is an accepted listener.
type listener struct {
	gwv1.Listener
	target int
	cert   *config.Certificate
	serves bool // false: accepted, but its certificate is unusable
}

func serviceName(g *gwv1.Gateway) string {
	// The UID is in the name so a recreated Gateway does not inherit the old one's ports.
	sum := sha256.Sum256([]byte(g.Namespace + "/" + g.Name + "/" + string(g.UID)))
	return "gw-" + hex.EncodeToString(sum[:10])
}

// managed reports whether svc is a Gateway Service this controller made.
func managed(svc *corev1.Service, systemNS string) bool {
	return svc.Namespace == systemNS && svc.Labels[managedByLabel] == managedByValue
}

// reservePorts records the target ports already given out: the Services are
// the only durable record of them.
func (t *translator) reservePorts() {
	for i := range t.in.Services {
		s := &t.in.Services[i]
		if !managed(s, t.in.SystemNamespace) {
			continue
		}
		for _, p := range s.Spec.Ports {
			n := p.TargetPort.IntValue()
			if n >= firstTargetPort && n <= lastTargetPort && !t.usedPorts[n] {
				t.usedPorts[n] = true
				t.knownPorts[s.Name+"/"+p.Name] = n
			}
		}
	}
}

// targetPort returns key's port: the one it has, or a free one found from its hash.
func (t *translator) targetPort(key string) (int, bool) {
	if n, ok := t.knownPorts[key]; ok {
		return n, true
	}
	const span = lastTargetPort - firstTargetPort + 1
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	start := int(h.Sum32() % span)
	for i := range span {
		if n := firstTargetPort + (start+i)%span; !t.usedPorts[n] {
			t.usedPorts[n] = true
			t.knownPorts[key] = n
			return n, true
		}
	}
	return 0, false
}

func supportedProtocol(p gwv1.ProtocolType) bool {
	return p == gwv1.HTTPProtocolType || p == gwv1.HTTPSProtocolType
}

// conflict returns why l cannot share the Gateway with its other listeners.
func conflict(all []gwv1.Listener, l gwv1.Listener) gwv1.ListenerConditionReason {
	for _, o := range all {
		if o.Name == l.Name || o.Port != l.Port || !supportedProtocol(o.Protocol) {
			continue
		}
		if o.Protocol != l.Protocol {
			return gwv1.ListenerReasonProtocolConflict
		}
		if strings.EqualFold(string(deref(o.Hostname, "")), string(deref(l.Hostname, ""))) {
			return gwv1.ListenerReasonHostnameConflict
		}
	}
	return ""
}

// gateway works out each listener's status, certificate and target port.
func (t *translator) gateway(g *gwv1.Gateway) {
	res := &GatewayResult{Service: serviceName(g), Programmable: map[gwv1.SectionName]bool{}}
	var ls []*listener
	for _, l := range g.Spec.Listeners {
		c := func(typ gwv1.ListenerConditionType, reason gwv1.ListenerConditionReason, ok bool, msg string) metav1.Condition {
			return cond(string(typ), string(reason), ok, g.Generation, msg)
		}
		st := gwv1.ListenerStatus{Name: l.Name}
		accepted := c(gwv1.ListenerConditionAccepted, gwv1.ListenerReasonAccepted, true, "")
		resolved := c(gwv1.ListenerConditionResolvedRefs, gwv1.ListenerReasonResolvedRefs, true, "")
		conflicted := c(gwv1.ListenerConditionConflicted, gwv1.ListenerReasonNoConflicts, false, "")
		reject := func(r gwv1.ListenerConditionReason, msg string) {
			accepted = c(gwv1.ListenerConditionAccepted, r, false, msg)
		}

		https := l.Protocol == gwv1.HTTPSProtocolType
		if !supportedProtocol(l.Protocol) {
			reject(gwv1.ListenerReasonUnsupportedProtocol, fmt.Sprintf("protocol %s is not supported; use HTTP or HTTPS", l.Protocol))
		} else {
			if allowsHTTPRoute(l) {
				st.SupportedKinds = []gwv1.RouteGroupKind{{Group: ptr(gwv1.Group(gwv1.GroupName)), Kind: "HTTPRoute"}}
			} else {
				st.SupportedKinds = []gwv1.RouteGroupKind{}
			}
			if invalidKinds(l) {
				resolved = c(gwv1.ListenerConditionResolvedRefs, gwv1.ListenerReasonInvalidRouteKinds, false, "only HTTPRoute is supported")
			}
			switch r := conflict(g.Spec.Listeners, l); {
			case r != "":
				reject(r, "conflicts with another listener on the same port")
				conflicted = c(gwv1.ListenerConditionConflicted, r, true, "conflicts with another listener on the same port")
			case https && l.TLS != nil && deref(l.TLS.Mode, gwv1.TLSModeTerminate) != gwv1.TLSModeTerminate:
				reject(gwv1.ListenerReasonUnsupportedValue, "only tls.mode Terminate is supported")
			case https && g.Spec.TLS != nil && g.Spec.TLS.Frontend != nil:
				// Serving without the client-certificate check that was asked for would fail open.
				reject(gwv1.ListenerReasonUnsupportedValue, "client certificate validation is not supported")
			default:
				name := fmt.Sprintf("%s-%d", strings.ToLower(string(l.Protocol)), l.Port)
				target, ok := t.targetPort(res.Service + "/" + name)
				if !ok {
					reject(gwv1.ListenerReasonPortUnavailable, "the data plane has no free port")
					break
				}
				if p := (GatewayPort{Name: name, Port: l.Port, Target: int32(target)}); !slices.Contains(res.Ports, p) { //nolint:gosec // target is within 10000..19999
					res.Ports = append(res.Ports, p)
				}
				nl := &listener{Listener: l, target: target, serves: true}
				if https {
					cert, reason, msg := t.certificate(g, l)
					if reason != "" {
						resolved = c(gwv1.ListenerConditionResolvedRefs, reason, false, msg)
						nl.serves = false
					}
					nl.cert = cert
				}
				res.Programmable[l.Name] = nl.serves
				ls = append(ls, nl)
			}
		}
		st.Conditions = []metav1.Condition{accepted, resolved, conflicted}
		res.Listeners = append(res.Listeners, st)
	}

	serving := 0
	for _, l := range ls {
		if l.serves {
			serving++
		}
	}
	ok, reason, msg := len(ls) > 0, string(gwv1.GatewayReasonAccepted), ""
	switch {
	case !ok:
		reason, msg = string(gwv1.GatewayReasonListenersNotValid), "no listener can be accepted"
	case serving < len(g.Spec.Listeners): // still Accepted, but the spec wants the reason to flag the bad listeners
		reason, msg = string(gwv1.GatewayReasonListenersNotValid), "some listeners are invalid; see their conditions"
	}
	res.Conditions = []metav1.Condition{cond(string(gwv1.GatewayConditionAccepted), reason, ok, g.Generation, msg)}
	gk := nsName(g)
	t.out.Gateways[gk] = res
	if ok {
		t.gateways[gk] = g
		t.listeners[gk] = ls
	}
}

// certRef returns the Secret an HTTPS listener names, or why it may not use it.
func certRef(grants []gwv1.ReferenceGrant, g *gwv1.Gateway, l gwv1.Listener) (types.NamespacedName, gwv1.ListenerConditionReason, string) {
	var none types.NamespacedName
	if l.TLS == nil || len(l.TLS.CertificateRefs) != 1 {
		return none, gwv1.ListenerReasonInvalidCertificateRef, "exactly one certificateRef is supported"
	}
	ref := l.TLS.CertificateRefs[0]
	if deref(ref.Group, "") != "" || deref(ref.Kind, "Secret") != "Secret" {
		return none, gwv1.ListenerReasonInvalidCertificateRef, "only core Secrets are supported"
	}
	key := types.NamespacedName{Namespace: string(deref(ref.Namespace, gwv1.Namespace(g.Namespace))), Name: string(ref.Name)}
	// The client refuses a malformed name, and that error would fail every Gateway's reconcile.
	if len(validation.IsDNS1123Subdomain(key.Name)) > 0 || len(validation.IsDNS1123Label(key.Namespace)) > 0 {
		return none, gwv1.ListenerReasonInvalidCertificateRef, "not a valid Secret name"
	}
	if key.Namespace != g.Namespace && !granted(grants, "Gateway", g.Namespace, "Secret", key.Namespace, key.Name) {
		return none, gwv1.ListenerReasonRefNotPermitted, fmt.Sprintf("secret %s: no ReferenceGrant allows this cross-namespace reference", key)
	}
	return key, "", ""
}

// secretRefs lists the Secrets Translate will need: only those a listener of
// our Gateways names and is permitted to use.
func secretRefs(classes []gwv1.GatewayClass, gateways []gwv1.Gateway, grants []gwv1.ReferenceGrant) []types.NamespacedName {
	var out []types.NamespacedName
	for i := range gateways {
		g := &gateways[i]
		if !slices.ContainsFunc(classes, func(c gwv1.GatewayClass) bool {
			return c.Name == string(g.Spec.GatewayClassName) && c.Spec.ControllerName == ControllerName
		}) {
			continue
		}
		for _, l := range g.Spec.Listeners {
			if l.Protocol != gwv1.HTTPSProtocolType {
				continue
			}
			if key, reason, _ := certRef(grants, g, l); reason == "" && !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	return out
}

func (t *translator) certificate(g *gwv1.Gateway, l gwv1.Listener) (*config.Certificate, gwv1.ListenerConditionReason, string) {
	key, reason, msg := certRef(t.in.Grants, g, l)
	if reason != "" {
		return nil, reason, msg
	}
	s := t.in.Secrets[key]
	if s == nil {
		return nil, gwv1.ListenerReasonInvalidCertificateRef, fmt.Sprintf("secret %s was not found", key)
	}
	crt, pk := s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]
	if len(crt)+len(pk) > maxCertBytes {
		return nil, gwv1.ListenerReasonInvalidCertificateRef, fmt.Sprintf("secret %s is over %d bytes", key, maxCertBytes)
	}
	if _, err := tls.X509KeyPair(crt, pk); err != nil {
		// Not err itself: nothing derived from key material belongs in a status.
		return nil, gwv1.ListenerReasonInvalidCertificateRef, fmt.Sprintf("secret %s is not a valid certificate and key pair", key)
	}
	return &config.Certificate{Host: strings.ToLower(string(deref(l.Hostname, ""))), Cert: string(crt), Key: string(pk)}, "", ""
}

// configListeners lists the ports the data plane should have open.
func (t *translator) configListeners() []config.Listener {
	byPort := map[int]*config.Listener{}
	for _, ls := range t.listeners {
		for _, l := range ls {
			if !l.serves {
				continue
			}
			cl := byPort[l.target]
			if cl == nil {
				cl = &config.Listener{Port: l.target}
				byPort[l.target] = cl
			}
			if l.cert != nil {
				cl.Certificates = append(cl.Certificates, *l.cert)
			}
		}
	}
	var out []config.Listener
	for _, cl := range byPort {
		out = append(out, *cl)
	}
	slices.SortFunc(out, func(a, b config.Listener) int { return cmp.Compare(a.Port, b.Port) })
	return out
}
