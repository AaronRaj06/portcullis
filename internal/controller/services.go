package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type serviceState struct {
	addresses []gwv1.GatewayStatusAddress
	err       error
}

// syncServices makes one Service per Gateway and deletes those whose Gateway is
// gone. One Gateway's failure does not stop the others.
func (r *Reconciler) syncServices(ctx context.Context, out *Output, services []corev1.Service) (map[types.NamespacedName]serviceState, error) {
	have := map[string]*corev1.Service{}
	var selector map[string]string
	for i := range services {
		s := &services[i]
		if nsName(s) == r.ProxyService {
			selector = s.Spec.Selector
		}
		if managed(s, r.ProxyService.Namespace) {
			have[s.Name] = s
		}
	}
	states := map[types.NamespacedName]serviceState{}
	keep := map[string]bool{}
	var errs []error
	for gk, res := range out.Gateways {
		if len(res.Ports) == 0 {
			continue
		}
		keep[res.Service] = true
		svc, err := r.ensureService(ctx, gk, res, have[res.Service], selector)
		st := serviceState{err: err}
		if err != nil {
			errs = append(errs, err)
		}
		if svc != nil {
			st.addresses = addresses(svc)
		}
		states[gk] = st
	}
	for name, s := range have {
		if !keep[name] {
			if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete service %s: %w", name, err))
			}
		}
	}
	return states, errors.Join(errs...)
}

func (r *Reconciler) ensureService(ctx context.Context, gk types.NamespacedName, res *GatewayResult, cur *corev1.Service,
	selector map[string]string) (*corev1.Service, error) {
	if len(selector) == 0 {
		return cur, fmt.Errorf("service %s has no selector to copy", r.ProxyService)
	}
	var ports []corev1.ServicePort
	for _, p := range res.Ports {
		sp := corev1.ServicePort{Name: p.Name, Protocol: corev1.ProtocolTCP, Port: p.Port, TargetPort: intstr.FromInt32(p.Target)}
		if cur != nil {
			for _, old := range cur.Spec.Ports {
				if old.Name == p.Name {
					sp.NodePort = old.NodePort // assigned if someone made it a NodePort or LoadBalancer
				}
			}
		}
		ports = append(ports, sp)
	}
	if cur == nil {
		s := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: r.ProxyService.Namespace, Name: res.Service,
				Labels:      map[string]string{managedByLabel: managedByValue},
				Annotations: map[string]string{gatewayAnnotation: gk.String()},
			},
			Spec: corev1.ServiceSpec{Selector: selector, Ports: ports},
		}
		if err := r.Create(ctx, s); err != nil {
			return nil, fmt.Errorf("create service for gateway %s: %w", gk, err)
		}
		return s, nil
	}
	same := maps.Equal(cur.Spec.Selector, selector) && slices.EqualFunc(cur.Spec.Ports, ports, func(a, b corev1.ServicePort) bool {
		return a.Name == b.Name && a.Port == b.Port && a.TargetPort == b.TargetPort
	})
	if same {
		return cur, nil
	}
	s := cur.DeepCopy()
	s.Spec.Selector, s.Spec.Ports = selector, ports
	if err := r.Update(ctx, s); err != nil {
		return cur, fmt.Errorf("update service for gateway %s: %w", gk, err)
	}
	return s, nil
}

func addresses(s *corev1.Service) []gwv1.GatewayStatusAddress {
	var out []gwv1.GatewayStatusAddress
	for _, in := range s.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			out = append(out, gwv1.GatewayStatusAddress{Type: ptr(gwv1.IPAddressType), Value: in.IP})
		}
	}
	if len(out) == 0 && s.Spec.ClusterIP != "" && s.Spec.ClusterIP != corev1.ClusterIPNone {
		out = append(out, gwv1.GatewayStatusAddress{Type: ptr(gwv1.IPAddressType), Value: s.Spec.ClusterIP})
	}
	return out
}
