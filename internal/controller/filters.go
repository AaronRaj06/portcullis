package controller

import (
	"strings"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
)

// ruleFilters translates a rule's filters, or names the one it cannot honour.
// The redirect's scheme and port are filled in per listener.
func ruleFilters(rule gwv1.HTTPRouteRule) (*config.HeaderEdit, *config.Redirect, string) {
	for _, br := range rule.BackendRefs {
		if len(br.Filters) > 0 {
			return nil, nil, "backendRef filters are not supported"
		}
	}
	var edit *config.HeaderEdit
	var redir *config.Redirect
	for _, f := range rule.Filters {
		switch {
		case f.Type == gwv1.HTTPRouteFilterRequestHeaderModifier && f.RequestHeaderModifier != nil && edit == nil:
			edit = &config.HeaderEdit{
				Set:    headerValues(f.RequestHeaderModifier.Set),
				Add:    headerValues(f.RequestHeaderModifier.Add),
				Remove: f.RequestHeaderModifier.Remove,
			}
			if err := edit.Check(); err != nil {
				return nil, nil, err.Error()
			}
		case f.Type == gwv1.HTTPRouteFilterRequestRedirect && f.RequestRedirect != nil && redir == nil:
			d := f.RequestRedirect
			if d.Scheme != nil || d.Port != nil || d.Path != nil {
				return nil, nil, "redirect scheme, port and path are not supported"
			}
			redir = &config.Redirect{Status: deref(d.StatusCode, 302), Host: string(deref(d.Hostname, ""))}
			if redir.Status != 301 && redir.Status != 302 {
				return nil, nil, "only 301 and 302 redirects are supported"
			}
			if strings.ContainsAny(redir.Host, " /?#@\\\r\n") {
				return nil, nil, "the redirect hostname is invalid"
			}
		default:
			return nil, nil, "filter " + string(f.Type) + " is not supported"
		}
	}
	return edit, redir, ""
}

func headerValues(hs []gwv1.HTTPHeader) []config.HeaderMatch {
	var out []config.HeaderMatch
	for _, h := range hs {
		out = append(out, config.HeaderMatch{Name: string(h.Name), Value: h.Value})
	}
	return out
}
