// Package v1alpha1 defines the PortcullisPolicy custom resource
// (portcullis.dev/v1alpha1): a PolicyScript program attached to a Gateway or
// an HTTPRoute. The CRD manifest is deploy/crds/portcullis.dev_portcullispolicies.yaml.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is portcullis.dev/v1alpha1.
var GroupVersion = schema.GroupVersion{Group: "portcullis.dev", Version: "v1alpha1"}

// AddToScheme registers the types with a scheme.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &PortcullisPolicy{}, &PortcullisPolicyList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// PortcullisPolicy attaches a PolicyScript program to a Gateway (every route
// through it) or to one HTTPRoute, in the policy's own namespace.
type PortcullisPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicySpec   `json:"spec"`
	Status PolicyStatus `json:"status,omitempty"`
}

// PolicySpec is the desired policy.
type PolicySpec struct {
	TargetRef TargetRef `json:"targetRef"`
	Script    string    `json:"script"`
	// OnError is fail_closed (default) or fail_open.
	OnError string `json:"onError,omitempty"`
}

// TargetRef names a Gateway or HTTPRoute in the policy's namespace
// (Gateway API "policy attachment").
type TargetRef struct {
	Group string `json:"group"` // gateway.networking.k8s.io
	Kind  string `json:"kind"`  // Gateway or HTTPRoute
	Name  string `json:"name"`
}

// PolicyStatus reports whether the script compiled and attached.
type PolicyStatus struct {
	// Conditions: Accepted (reasons Accepted, Invalid, TargetNotFound, Conflicted).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PortcullisPolicyList is a list of PortcullisPolicy.
type PortcullisPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PortcullisPolicy `json:"items"`
}

// DeepCopyInto copies p into out. (Hand-written: three plain structs do not justify code generation.)
func (p *PortcullisPolicy) DeepCopyInto(out *PortcullisPolicy) {
	*out = *p
	p.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if p.Status.Conditions != nil {
		out.Status.Conditions = make([]metav1.Condition, len(p.Status.Conditions))
		for i := range p.Status.Conditions {
			p.Status.Conditions[i].DeepCopyInto(&out.Status.Conditions[i])
		}
	}
}

// DeepCopy returns a deep copy of p.
func (p *PortcullisPolicy) DeepCopy() *PortcullisPolicy {
	if p == nil {
		return nil
	}
	out := new(PortcullisPolicy)
	p.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (p *PortcullisPolicy) DeepCopyObject() runtime.Object { return p.DeepCopy() }

// DeepCopyObject implements runtime.Object.
func (l *PortcullisPolicyList) DeepCopyObject() runtime.Object {
	if l == nil {
		return nil
	}
	out := &PortcullisPolicyList{TypeMeta: l.TypeMeta}
	l.DeepCopyInto(&out.ListMeta) // ListMeta's, promoted
	out.Items = make([]PortcullisPolicy, len(l.Items))
	for i := range l.Items {
		l.Items[i].DeepCopyInto(&out.Items[i])
	}
	return out
}
