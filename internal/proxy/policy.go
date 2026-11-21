package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/AaronRaj06/portcullis/internal/policy"
)

// requestPolicies runs each policy's request handler in order. It returns true
// if the request has been answered (deny, redirect, or a fail-closed error).
func (h *Handler) requestPolicies(w http.ResponseWriter, rc *http.ResponseController, progs []*policy.Program,
	env *policy.Env, rid string, st *reqState) bool {
	defer h.policyOutput(env, rid, st)
	for _, p := range progs {
		v, err := p.EvalRequest(env)
		if err != nil {
			if h.policyFailed(p, err, rid) {
				continue // fail open: as if this policy did not exist
			}
			st.status, st.errKind = http.StatusInternalServerError, "policy_error"
			h.writeError(w, rc, st.status, rid)
			return true
		}
		switch v.Kind {
		case policy.Allow:
			return false
		case policy.Deny:
			st.status, st.errKind = v.Status, "policy_deny"
			h.denied(p, v.Status)
			h.writeErrorMsg(w, rc, v.Status, rid, v.Message)
			return true
		case policy.Redirect:
			st.status, st.errKind = v.Status, "policy_redirect"
			h.denied(p, v.Status)
			h.extendWrite(rc)
			w.Header().Set("Location", v.Location)
			w.Header().Set(HeaderRequestID, rid)
			w.WriteHeader(v.Status)
			return true
		}
	}
	return false
}

// responsePolicies runs each policy's response handler. It returns false if
// a fail-closed policy failed, so the response must be replaced by a 500.
func (h *Handler) responsePolicies(progs []*policy.Program, env *policy.Env, rid string, st *reqState) bool {
	env.Tags, env.Logs = env.Tags[:0], env.Logs[:0] // the request phase's were already recorded
	defer h.policyOutput(env, rid, st)
	for _, p := range progs {
		if err := p.EvalResponse(env); err != nil && !h.policyFailed(p, err, rid) {
			return false
		}
	}
	return true
}

// policyFailed records a runtime error and reports whether to fail open.
func (h *Handler) policyFailed(p *policy.Program, err error, rid string) (failOpen bool) {
	if h.o.Metrics != nil {
		h.o.Metrics.PolicyError(p.Name)
	}
	h.o.Logger.Warn("policy error", "request_id", rid, "policy", p.Name, "fail_open", p.FailOpen, "error", err)
	return p.FailOpen
}

func (h *Handler) denied(p *policy.Program, status int) {
	if h.o.Metrics != nil {
		h.o.Metrics.PolicyDenial(p.Name, status)
	}
}

// policyOutput writes log(...) lines and keeps tag(...) values for the access log.
func (h *Handler) policyOutput(env *policy.Env, rid string, st *reqState) {
	st.tags = append(st.tags, env.Tags...)
	for _, m := range env.Logs {
		h.o.Logger.LogAttrs(context.Background(), slog.LevelInfo, "policy", slog.String("request_id", rid), slog.String("message", m))
	}
}
