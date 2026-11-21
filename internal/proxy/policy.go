package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

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

// envPool recycles policy environments: one per request on routes with policies.
var envPool = sync.Pool{New: func() any { return new(policy.Env) }}

// responsePhase lets the response policies see and change status/hdr before
// the client does. false: a fail-closed policy failed, so the caller must
// answer 500 instead.
func (h *Handler) responsePhase(st *reqState, status int, hdr http.Header, rid string) bool {
	if st.penv == nil {
		return true
	}
	st.penv.Status, st.penv.RespHeader = status, hdr
	return h.responsePolicies(st.progs, st.penv, rid, st)
}

// upstreamError answers a request that failed after routing (no backend,
// upstream error or timeout, client body error), after the response policies
// have seen it, so they can e.g. add Retry-After or Cache-Control to a 503.
func (h *Handler) upstreamError(w http.ResponseWriter, rc *http.ResponseController, rid string, st *reqState) {
	if !h.responsePhase(st, st.status, w.Header(), rid) {
		clear(w.Header())
		st.status, st.errKind = http.StatusInternalServerError, "policy_error"
	}
	h.writeError(w, rc, st.status, rid)
}

func (h *Handler) upgradeFailed(w http.ResponseWriter, rc *http.ResponseController, rid string, st *reqState, kind string) (int, string) {
	st.status, st.errKind = http.StatusBadGateway, kind
	h.upstreamError(w, rc, rid, st)
	return st.status, st.errKind
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
