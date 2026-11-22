package controller

import (
	"maps"
	"time"

	"context"
	"errors"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"net"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
)

// certRefresh is how often referenced Secrets are re-read. The controller may
// only `get` Secrets, so it cannot watch them, or list the cluster's.
const certRefresh = 15 * time.Second

// Reconciler rebuilds the whole desired state on every change. There is one
// work-queue key, so changes that arrive together are reconciled once, and the
// data planes always receive one consistent snapshot. Failed pushes are retried
// with the work queue's exponential back-off.
//
// shortcut: each reconcile lists every watched object (fine to thousands of
// routes); a very large cluster would want per-Gateway keys and indexes.
type Reconciler struct {
	client.Client
	Pusher       *Pusher
	AdminService types.NamespacedName // headless/not-ready-publishing Service selecting data-plane pods
	AdminPort    int32
	ProxyService types.NamespacedName // its selector and namespace are used for every Gateway's Service
	Secrets      client.Reader        // uncached, so that `get` is the only RBAC verb needed
}

var allKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "portcullis"}}

// SetupWithManager registers the watches: every object kind maps to allKey.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request { return []reconcile.Request{allKey} })
	spec := builder.WithPredicates(predicate.GenerationChangedPredicate{}) // our own status writes do not re-trigger
	return ctrl.NewControllerManagedBy(mgr).Named("portcullis").
		Watches(&gwv1.GatewayClass{}, all, spec).
		Watches(&gwv1.Gateway{}, all, spec).
		Watches(&gwv1.HTTPRoute{}, all, spec).
		Watches(&v1alpha1.PortcullisPolicy{}, all, spec).
		Watches(&gwv1.ReferenceGrant{}, all, spec).
		Watches(&corev1.Namespace{}, all). // labels feed allowedRoutes selectors
		Watches(&corev1.Service{}, all).
		Watches(&discoveryv1.EndpointSlice{}, all).
		Complete(r)
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	var (
		classes  gwv1.GatewayClassList
		gateways gwv1.GatewayList
		routes   gwv1.HTTPRouteList
		services corev1.ServiceList
		eps      discoveryv1.EndpointSliceList
		policies v1alpha1.PortcullisPolicyList
		grants   gwv1.ReferenceGrantList
		nss      corev1.NamespaceList
	)
	for _, l := range []client.ObjectList{&classes, &gateways, &routes, &services, &eps, &policies, &grants, &nss} {
		if err := r.List(ctx, l); err != nil {
			return reconcile.Result{}, err
		}
	}
	endpoints := r.dataPlanes(eps.Items)
	var again reconcile.Result
	secrets := map[types.NamespacedName]*corev1.Secret{}
	// shortcut: one GET per referenced Secret each reconcile; cache them if there are hundreds.
	for _, key := range secretRefs(classes.Items, gateways.Items, grants.Items) {
		again.RequeueAfter = certRefresh
		s := &corev1.Secret{}
		switch err := r.Secrets.Get(ctx, key, s); {
		case err == nil:
			secrets[key] = s
		case !apierrors.IsNotFound(err):
			return reconcile.Result{}, fmt.Errorf("secret %s: %w", key, err)
		}
	}
	out := Translate(Inputs{Secrets: secrets, SystemNamespace: r.ProxyService.Namespace,
		Classes: classes.Items, Gateways: gateways.Items, Routes: routes.Items, Services: services.Items,
		Slices: eps.Items, Policies: policies.Items, Grants: grants.Items, Namespaces: nss.Items, Instances: len(endpoints)})

	snap, err := NewSnapshot(out.Config)
	if err == nil {
		err = r.Pusher.Push(ctx, endpoints, snap)
	}
	if err == nil {
		log.Info("config pushed", "hash", snap.Hash[:12], "dataplanes", len(endpoints), "routes", len(out.Config.Routes))
	}
	var synced map[types.NamespacedName]serviceState
	var syncErr error
	if err == nil { // only after the push: a Service must not point at a port that is not open
		synced, syncErr = r.syncServices(ctx, out, services.Items)
	}
	statusErr := r.writeStatus(ctx, out, &classes, &gateways, &routes, &policies, synced, err)
	if err := errors.Join(err, syncErr, statusErr); err != nil {
		return reconcile.Result{}, err
	}
	return again, nil
}

// dataPlanes lists the admin endpoints of every data-plane pod. Not-ready pods
// are included: a new pod is unready exactly until it receives its first config.
func (r *Reconciler) dataPlanes(eps []discoveryv1.EndpointSlice) []string {
	var out []string
	for _, s := range eps {
		if s.Namespace != r.AdminService.Namespace || s.Labels[discoveryv1.LabelServiceName] != r.AdminService.Name {
			continue
		}
		for _, e := range s.Endpoints {
			if deref(e.Conditions.Terminating, false) {
				continue
			}
			for _, a := range e.Addresses {
				out = append(out, net.JoinHostPort(a, strconv.Itoa(int(r.AdminPort))))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// programmed is the Programmed condition for this push outcome.
func programmed(pushErr error, gen int64) metav1.Condition {
	if pushErr == nil {
		return cond(string(gwv1.GatewayConditionProgrammed), string(gwv1.GatewayReasonProgrammed), true, gen, "")
	}
	reason := string(gwv1.GatewayReasonPending)
	if errors.Is(pushErr, ErrNoDataPlane) {
		reason = string(gwv1.GatewayReasonNoResources)
	}
	msg := pushErr.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	return cond(string(gwv1.GatewayConditionProgrammed), reason, false, gen, msg)
}

// merge sets want on a copy of have, keeping lastTransitionTime of
// conditions whose status did not change.
func merge(have, want []metav1.Condition) []metav1.Condition {
	out := slices.Clone(have)
	for _, c := range want {
		meta.SetStatusCondition(&out, c)
	}
	return out
}

func (r *Reconciler) writeStatus(ctx context.Context, out *Output, classes *gwv1.GatewayClassList, gateways *gwv1.GatewayList,
	routes *gwv1.HTTPRouteList, policies *v1alpha1.PortcullisPolicyList, synced map[types.NamespacedName]serviceState, pushErr error) error {
	var errs []error
	update := func(obj client.Object, changed bool) {
		if changed {
			if err := r.Status().Update(ctx, obj); err != nil {
				errs = append(errs, fmt.Errorf("status of %T %s: %w", obj, client.ObjectKeyFromObject(obj), err))
			}
		}
	}
	for i := range classes.Items {
		c := &classes.Items[i]
		if want, ok := out.Classes[c.Name]; ok {
			next := merge(c.Status.Conditions, want)
			changed := !equality.Semantic.DeepEqual(next, c.Status.Conditions)
			c.Status.Conditions = next
			update(c, changed)
		}
	}
	for i := range gateways.Items {
		g := &gateways.Items[i]
		res, ok := out.Gateways[nsName(g)]
		if !ok {
			continue
		}
		next := g.Status.DeepCopy()
		accepted := meta.IsStatusConditionTrue(res.Conditions, string(gwv1.GatewayConditionAccepted))
		svc := synced[nsName(g)]
		prog := programmed(errors.Join(pushErr, svc.err), g.Generation)
		serves := slices.Contains(slices.Collect(maps.Values(res.Programmable)), true)
		switch {
		case accepted && !serves:
			prog = cond(string(gwv1.GatewayConditionProgrammed), string(gwv1.GatewayReasonInvalid), false, g.Generation, "no listener has a usable certificate")
		case !accepted:
			prog = cond(string(gwv1.GatewayConditionProgrammed), string(gwv1.GatewayReasonInvalid), false, g.Generation, "the Gateway is not accepted")
		}
		next.Conditions = merge(next.Conditions, append(slices.Clone(res.Conditions), prog))
		if pushErr == nil { // otherwise keep the address it already has
			next.Addresses = svc.addresses
		}
		listeners := make([]gwv1.ListenerStatus, len(res.Listeners))
		for j, ls := range res.Listeners {
			var have []metav1.Condition
			for _, old := range g.Status.Listeners {
				if old.Name == ls.Name {
					have = old.Conditions
				}
			}
			lp := prog
			if !res.Programmable[ls.Name] {
				lp = cond(string(gwv1.ListenerConditionProgrammed), string(gwv1.ListenerReasonInvalid), false, g.Generation, "listener is not valid; see its other conditions")
			}
			ls.Conditions = merge(have, append(slices.Clone(ls.Conditions), lp))
			listeners[j] = ls
		}
		next.Listeners = listeners
		changed := !equality.Semantic.DeepEqual(*next, g.Status)
		g.Status = *next
		update(g, changed)
	}
	for i := range routes.Items {
		rt := &routes.Items[i]
		want, ok := out.Routes[nsName(rt)]
		if !ok {
			continue
		}
		// Keep other controllers' parent statuses; replace ours.
		var parents []gwv1.RouteParentStatus
		for _, p := range rt.Status.Parents {
			if p.ControllerName != ControllerName {
				parents = append(parents, p)
			}
		}
		for _, w := range want {
			var have []metav1.Condition
			for _, p := range rt.Status.Parents {
				if p.ControllerName == ControllerName && equality.Semantic.DeepEqual(p.ParentRef, w.ParentRef) {
					have = p.Conditions
				}
			}
			conds := slices.Clone(w.Conditions)
			if meta.IsStatusConditionTrue(conds, string(gwv1.RouteConditionAccepted)) {
				// Not a core Gateway API route condition; added because the data plane ack is worth reporting.
				p := programmed(pushErr, rt.Generation)
				conds = append(conds, p)
			}
			w.Conditions = merge(have, conds)
			parents = append(parents, w)
		}
		changed := !equality.Semantic.DeepEqual(parents, rt.Status.Parents)
		rt.Status.Parents = parents
		update(rt, changed)
	}
	for i := range policies.Items {
		p := &policies.Items[i]
		want := out.Policies[nsName(p)]
		next := merge(p.Status.Conditions, want)
		changed := !equality.Semantic.DeepEqual(next, p.Status.Conditions)
		p.Status.Conditions = next
		update(p, changed)
	}
	return errors.Join(errs...)
}
