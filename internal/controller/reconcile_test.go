package controller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
)

// fakeDataPlane acts like the admin API: it validates and acknowledges a push.
type fakeDataPlane struct {
	mu     sync.Mutex
	last   *config.Config
	fail   bool
	tokens []string
}

func (f *fakeDataPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	if f.fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	c, err := config.Parse(body)
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.last = c
	_ = json.NewEncoder(w).Encode(config.Status{Hash: c.Hash(), Version: c.Version, Changed: true})
}

func scheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, gwv1.Install, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestReconcilePushesAndWritesStatus(t *testing.T) {
	dp := &fakeDataPlane{}
	srv := httptest.NewServer(dp)
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	admin := slice("portcullis-admin", 9901, false, host) // not ready: still pushed to
	admin.Namespace = "portcullis-system"
	proxySvc := corev1.Service{ObjectMeta: om("portcullis-system", "portcullis"), Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.50"}}
	gc, gw := class(), gateway()
	route := httpRoute("app", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}})
	pol := v1alpha1.PortcullisPolicy{ObjectMeta: om("default", "block"), Spec: v1alpha1.PolicySpec{
		TargetRef: v1alpha1.TargetRef{Group: gwv1.GroupName, Kind: "HTTPRoute", Name: "app"},
		Script:    `policy "block" { on request { if path.starts_with("/admin") { deny(403) } } }`}}
	svc, eps := service("web", 80), slice("web", 8080, true, "10.0.0.1")
	objs := []client.Object{&gc, &gw, &route, &pol, &svc, &eps, &admin, &proxySvc}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gwv1.GatewayClass{}, &gwv1.Gateway{}, &gwv1.HTTPRoute{}, &v1alpha1.PortcullisPolicy{}).Build()
	r := &Reconciler{Client: c, Pusher: &Pusher{Client: srv.Client(), Token: "t0ken"},
		AdminService: types.NamespacedName{Namespace: "portcullis-system", Name: "portcullis-admin"}, AdminPort: int32(port),
		ProxyService: types.NamespacedName{Namespace: "portcullis-system", Name: "portcullis"}}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, allKey); err != nil {
		t.Fatal(err)
	}

	if dp.last == nil || len(dp.last.Policies) != 1 || dp.last.Instances != 1 || dp.tokens[0] != "Bearer t0ken" ||
		!strings.HasPrefix(dp.last.Version, "sha256:") {
		t.Fatalf("pushed: %+v tokens %v", dp.last, dp.tokens)
	}
	var g gwv1.Gateway
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gw"}, &g)
	if !meta.IsStatusConditionTrue(g.Status.Conditions, "Programmed") || !meta.IsStatusConditionTrue(g.Status.Conditions, "Accepted") ||
		len(g.Status.Addresses) != 1 || g.Status.Addresses[0].Value != "10.96.0.50" || g.Status.Listeners[0].AttachedRoutes != 1 ||
		!meta.IsStatusConditionTrue(g.Status.Listeners[0].Conditions, "Programmed") {
		t.Fatalf("gateway status: %+v", g.Status)
	}
	var rt gwv1.HTTPRoute
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app"}, &rt)
	if len(rt.Status.Parents) != 1 || !meta.IsStatusConditionTrue(rt.Status.Parents[0].Conditions, "Programmed") {
		t.Fatalf("route status: %+v", rt.Status)
	}
	var p v1alpha1.PortcullisPolicy
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "block"}, &p)
	if !meta.IsStatusConditionTrue(p.Status.Conditions, "Accepted") {
		t.Fatalf("policy status: %+v", p.Status)
	}
	var gcs gwv1.GatewayClass
	_ = c.Get(ctx, types.NamespacedName{Name: "portcullis"}, &gcs)
	if !meta.IsStatusConditionTrue(gcs.Status.Conditions, "Accepted") {
		t.Fatalf("class status: %+v", gcs.Status)
	}
	before := g.Status.Conditions[0].LastTransitionTime

	// A refused push: Programmed turns False with the reason, and the error
	// makes the work queue retry with back-off.
	dp.mu.Lock()
	dp.fail = true
	dp.mu.Unlock()
	if _, err := r.Reconcile(ctx, allKey); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want push error, got %v", err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gw"}, &g)
	pc := meta.FindStatusCondition(g.Status.Conditions, "Programmed")
	if pc.Status != metav1.ConditionFalse || pc.Reason != "Pending" || !strings.Contains(pc.Message, "boom") {
		t.Fatalf("programmed after failure: %+v", pc)
	}
	if !meta.FindStatusCondition(g.Status.Conditions, "Accepted").LastTransitionTime.Equal(&before) {
		t.Fatal("an unchanged condition must keep its lastTransitionTime")
	}
}

func TestNoDataPlane(t *testing.T) {
	gc, gw := class(), gateway()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(&gc, &gw).
		WithStatusSubresource(&gwv1.GatewayClass{}, &gwv1.Gateway{}).Build()
	r := &Reconciler{Client: c, Pusher: &Pusher{Client: http.DefaultClient}, AdminService: types.NamespacedName{Namespace: "x", Name: "y"}, AdminPort: 9901}
	if _, err := r.Reconcile(context.Background(), allKey); err == nil {
		t.Fatal("want ErrNoDataPlane")
	}
	var g gwv1.Gateway
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "gw"}, &g)
	if pc := meta.FindStatusCondition(g.Status.Conditions, "Programmed"); pc == nil || pc.Reason != "NoResources" {
		t.Fatalf("programmed: %+v", pc)
	}
}
