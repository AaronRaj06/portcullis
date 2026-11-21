package policy

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/AaronRaj06/portcullis/internal/ratelimit"
)

// Defaults for the per-evaluation budgets.
const (
	DefaultMaxSteps = 10_000
	DefaultTimeout  = time.Millisecond
)

// Options configure Compile.
type Options struct {
	MaxSteps int           // 0: DefaultMaxSteps
	Timeout  time.Duration // 0: DefaultTimeout
	FailOpen bool          // on a runtime error, skip the policy instead of answering 500
	// Share divides every rate_limit by the number of proxy replicas that
	// split the traffic (0 or 1: no division).
	Share int
	// Pools, if non-nil, is the set of valid route_to targets.
	Pools map[string]bool
	// Limiter returns the limiter for one rate_limit call site. site identifies
	// the policy, position, limit and window, so a caller can hand back the
	// same limiter across hot reloads. nil: a fresh limiter per site.
	Limiter func(site string, limit int, window time.Duration) *ratelimit.Limiter
}

func (o Options) limiter(site string, limit int, window time.Duration) *ratelimit.Limiter {
	if o.Limiter != nil {
		return o.Limiter(site, limit, window)
	}
	return ratelimit.New(limit, window, 0)
}

// Program is a compiled policy. It is immutable and safe for concurrent use.
type Program struct {
	Name      string
	FailOpen  bool
	maxSteps  int
	timeout   time.Duration
	req, resp []*node
	hasReq    bool
	hasResp   bool
}

// HasRequest reports whether the policy has an "on request" handler.
func (p *Program) HasRequest() bool { return p.hasReq }

// HasResponse reports whether the policy has an "on response" handler.
func (p *Program) HasResponse() bool { return p.hasResp }

// Compile parses, checks and prepares src. Errors are *Error (syntax) or
// ErrorList (every type error), with line:col positions.
func Compile(src string, o Options) (*Program, error) {
	s, err := parse(src)
	if err != nil {
		return nil, err
	}
	if o.MaxSteps <= 0 {
		o.MaxSteps = DefaultMaxSteps
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	c := &checker{name: s.name, o: o, pools: o.Pools}
	p := &Program{Name: s.name, FailOpen: o.FailOpen, maxSteps: o.MaxSteps, timeout: o.Timeout, hasReq: s.hasReq, hasResp: s.hasResp}
	p.req = c.stmts(s.request)
	c.response = true
	p.resp = c.stmts(s.response)
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	return p, nil
}

// Env is one request as policies see it. The proxy fills it once per request
// and reuses it for every policy and both phases.
type Env struct {
	Method   string
	Host     string // lower case, no port
	Path     string // updated by rewrite_path
	RawQuery string
	ClientIP netip.Addr
	Now      int64 // request arrival, Unix seconds: what now() returns
	// Header is the request header. In the request phase it is the outbound
	// (upstream) header and set_header/remove_header change it.
	Header http.Header

	// Response phase.
	Status     int
	RespHeader http.Header // set_header/remove_header change it

	// Accumulated across policies, read by the proxy afterwards.
	RouteTo string   // the pool chosen by route_to, or ""
	Tags    []string // tag(...)
	Logs    []string // "policy: message" from log(...)

	query map[string][]string // parsed on first query(...)
}

// Reset clears e for reuse, keeping the capacity of its slices.
func (e *Env) Reset() { *e = Env{Tags: e.Tags[:0], Logs: e.Logs[:0]} }

// VerdictKind is how a request phase ended.
type VerdictKind uint8

// Verdict kinds.
const (
	Continue VerdictKind = iota // no terminal action: keep evaluating policies
	Allow                       // allow(): skip the remaining policies
	Deny                        // deny(status, message)
	Redirect                    // redirect(url, status)
)

// Verdict is the outcome of a request phase.
type Verdict struct {
	Kind     VerdictKind
	Status   int
	Message  string // Deny
	Location string // Redirect
}

// RuntimeError is a budget breach or an invalid value during evaluation.
type RuntimeError struct {
	Policy string
	Pos    string
	Msg    string
}

func (e *RuntimeError) Error() string {
	return fmt.Sprintf("policy %q at %s: %s", e.Policy, e.Pos, e.Msg)
}

// ErrBudget is wrapped by budget-breach RuntimeErrors.
var ErrBudget = errors.New("budget exceeded")

// EvalRequest runs the request handler. Its effects (headers, path, pool,
// tags, logs) are applied to env only if it completes without error.
func (p *Program) EvalRequest(env *Env) (Verdict, error) {
	if !p.hasReq {
		return Verdict{}, nil
	}
	st := state{p: p, env: env, path: env.Path, routeTo: env.RouteTo, target: env.Header, start: time.Now()}
	if err := st.block(p.req); err != nil && !errors.Is(err, errStop) {
		return Verdict{}, err
	}
	st.commit()
	return st.verdict, nil
}

// EvalResponse runs the response handler; effects as for EvalRequest.
func (p *Program) EvalResponse(env *Env) error {
	if !p.hasResp {
		return nil
	}
	st := state{p: p, env: env, path: env.Path, routeTo: env.RouteTo, target: env.RespHeader, response: true, start: time.Now()}
	if err := st.block(p.resp); err != nil && !errors.Is(err, errStop) {
		return err
	}
	st.commit()
	return nil
}
