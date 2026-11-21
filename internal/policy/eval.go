package policy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// value is one runtime value. Lists never exist at runtime: they only appear
// as literal right-hand sides of "in", resolved by the checker.
type value struct {
	s string
	i int64 // ints, and bools as 0/1
}

type hdrOp struct {
	name, value string
	remove      bool
}

// state is one evaluation. Effects are buffered and applied by commit, so a
// failed evaluation leaves the request untouched.
type state struct {
	p        *Program
	env      *Env
	steps    int
	start    time.Time
	target   http.Header // where set/remove go: env.Header (request) or env.RespHeader (response)
	response bool
	ops      [8]hdrOp // buffered header operations; a fixed array keeps state on the stack
	nops     int
	more     []hdrOp // operations beyond the first 8 (rare)
	path     string
	routeTo  string
	tags     []string
	logs     []string
	verdict  Verdict
}

var errStop = errors.New("stop") // a terminal action ended evaluation; not an error

func (st *state) fail(n *node, msg string) error {
	return &RuntimeError{Policy: st.p.Name, Pos: n.p.String(), Msg: msg}
}

// step charges one unit of the budget and checks the clock every 64 steps.
func (st *state) step(n *node) error {
	st.steps++
	if st.steps > st.p.maxSteps {
		return &RuntimeError{Policy: st.p.Name, Pos: n.p.String(), Msg: ErrBudget.Error() + ": steps"}
	}
	if st.steps&63 == 0 && time.Since(st.start) > st.p.timeout {
		return &RuntimeError{Policy: st.p.Name, Pos: n.p.String(), Msg: ErrBudget.Error() + ": time"}
	}
	return nil
}

func (st *state) commit() {
	h := st.target
	if h == nil && st.nops > 0 { // a caller that passed no header map still gets its effects
		h = http.Header{}
		if st.response {
			st.env.RespHeader = h
		} else {
			st.env.Header = h
		}
	}
	for i := range st.nops {
		op := st.op(i)
		if op.remove {
			delete(h, op.name)
		} else {
			h[op.name] = []string{op.value}
		}
	}
	st.env.Path, st.env.RouteTo = st.path, st.routeTo
	st.env.Tags = append(st.env.Tags, st.tags...)
	st.env.Logs = append(st.env.Logs, st.logs...)
}

func (st *state) block(ns []*node) error {
	for _, n := range ns {
		if err := st.exec(n); err != nil {
			return err
		}
	}
	return nil
}

func (st *state) exec(n *node) error {
	if err := st.step(n); err != nil {
		return err
	}
	switch n.op {
	case opIf:
		c, err := st.eval(n.kids[0])
		if err != nil {
			return err
		}
		if c.i != 0 {
			return st.block(n.then)
		}
		return st.block(n.els)
	case opDeny:
		st.verdict = Verdict{Kind: Deny, Status: int(n.i), Message: http.StatusText(int(n.i))}
		if len(n.kids) == 1 {
			m, err := st.eval(n.kids[0])
			if err != nil {
				return err
			}
			st.verdict.Message = m.s
		}
		return errStop
	case opAllow:
		st.verdict = Verdict{Kind: Allow}
		return errStop
	case opRedirect:
		u, err := st.eval(n.kids[0])
		if err != nil {
			return err
		}
		if !httpguts.ValidHeaderFieldValue(u.s) || u.s == "" {
			return st.fail(n, "redirect URL is empty or contains invalid characters")
		}
		st.verdict = Verdict{Kind: Redirect, Status: int(n.i), Location: u.s}
		return errStop
	case opRouteTo:
		st.routeTo = n.s
	case opRewrite:
		v, err := st.eval(n.kids[0])
		if err != nil {
			return err
		}
		if !strings.HasPrefix(v.s, "/") || strings.ContainsAny(v.s, "?#\x00\r\n") {
			return st.fail(n, "rewrite_path "+quote(v.s)+" must start with / and contain no ?, #, NUL, CR or LF")
		}
		st.path = v.s
	case opSetHeader:
		v, err := st.eval(n.kids[0])
		if err != nil {
			return err
		}
		if !httpguts.ValidHeaderFieldValue(v.s) {
			return st.fail(n, "set_header "+n.s+": value contains invalid characters (CR, LF or NUL)")
		}
		st.addOp(hdrOp{name: n.s, value: v.s})
	case opRemoveHeader:
		st.addOp(hdrOp{name: n.s, remove: true})
	case opLog:
		v, err := st.eval(n.kids[0])
		if err != nil {
			return err
		}
		st.logs = append(st.logs, st.p.Name+": "+v.s)
	case opTag:
		st.tags = append(st.tags, n.s)
	}
	return nil
}

func (st *state) addOp(op hdrOp) {
	if st.nops < len(st.ops) {
		st.ops[st.nops] = op
	} else {
		st.more = append(st.more, op)
	}
	st.nops++
}

func (st *state) op(i int) hdrOp {
	if i < len(st.ops) {
		return st.ops[i]
	}
	return st.more[i-len(st.ops)]
}

func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return "\"" + s + "\""
}

func b2v(b bool) value {
	if b {
		return value{i: 1}
	}
	return value{}
}

// header reads a header as this evaluation sees it: buffered set/remove
// operations from this policy come first, then the committed header.
func (st *state) header(h http.Header, canon string, own bool) string {
	if own {
		for i := st.nops - 1; i >= 0; i-- {
			if op := st.op(i); op.name == canon {
				if op.remove {
					return ""
				}
				return op.value
			}
		}
	}
	if v := h[canon]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (st *state) eval(n *node) (value, error) {
	if err := st.step(n); err != nil {
		return value{}, err
	}
	env := st.env
	switch n.op {
	case opStr:
		return value{s: n.s}, nil
	case opInt, opBool:
		return value{i: n.i}, nil
	case opMethod:
		return value{s: env.Method}, nil
	case opHost:
		return value{s: env.Host}, nil
	case opPath:
		return value{s: st.path}, nil
	case opClientIP:
		return value{}, nil // ip values are only used by "in" and rate_limit, which read env directly
	case opStatus:
		return value{i: int64(env.Status)}, nil
	case opHeader, opRespHeader:
		name := n.s
		if name == "" {
			v, err := st.eval(n.kids[0])
			if err != nil {
				return value{}, err
			}
			name = http.CanonicalHeaderKey(v.s)
		}
		if n.op == opHeader {
			return value{s: st.header(env.Header, name, !st.response)}, nil
		}
		return value{s: st.header(env.RespHeader, name, st.response)}, nil
	case opQuery:
		k, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		if env.query == nil {
			env.query, _ = url.ParseQuery(env.RawQuery) // keeps what parses; bad pairs are dropped
			if env.query == nil {
				env.query = url.Values{}
			}
		}
		if v := env.query[k.s]; len(v) > 0 {
			return value{s: v[0]}, nil
		}
		return value{}, nil
	case opCookie:
		k, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		return value{s: cookie(env.Header, k.s)}, nil
	case opStartsWith, opEndsWith, opContains, opConcat, opEq, opNe, opLt, opLe, opGt, opGe:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		y, err := st.eval(n.kids[1])
		if err != nil {
			return value{}, err
		}
		switch n.op {
		case opStartsWith:
			return b2v(strings.HasPrefix(x.s, y.s)), nil
		case opEndsWith:
			return b2v(strings.HasSuffix(x.s, y.s)), nil
		case opContains:
			return b2v(strings.Contains(x.s, y.s)), nil
		case opConcat:
			if len(x.s)+len(y.s) > MaxStringLit*4 {
				return value{}, st.fail(n, "string too long")
			}
			return value{s: x.s + y.s}, nil
		case opEq:
			return b2v(x.s == y.s && x.i == y.i), nil
		case opNe:
			return b2v(x.s != y.s || x.i != y.i), nil
		case opLt:
			return b2v(x.i < y.i), nil
		case opLe:
			return b2v(x.i <= y.i), nil
		case opGt:
			return b2v(x.i > y.i), nil
		default:
			return b2v(x.i >= y.i), nil
		}
	case opLower, opUpper, opLen:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		switch n.op {
		case opLower:
			return value{s: strings.ToLower(x.s)}, nil
		case opUpper:
			return value{s: strings.ToUpper(x.s)}, nil
		default:
			return value{i: int64(len(x.s))}, nil
		}
	case opInStrs, opInInts:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		for _, s := range n.strs {
			if s == x.s {
				return value{i: 1}, nil
			}
		}
		for _, i := range n.ints {
			if i == x.i {
				return value{i: 1}, nil
			}
		}
		return value{}, nil
	case opInNets:
		ip := env.ClientIP.Unmap()
		for _, p := range n.nets {
			if p.Contains(ip) {
				return value{i: 1}, nil
			}
		}
		return value{}, nil
	case opAnd, opOr:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		if (x.i != 0) == (n.op == opOr) { // short-circuit
			return x, nil
		}
		return st.eval(n.kids[1])
	case opNot:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		return b2v(x.i == 0), nil
	case opRateLimit:
		if n.kids[0].t == tyIP {
			var buf [64]byte // an address in text form fits, so a known client costs no allocation
			return b2v(!n.lim.AllowBytes(env.ClientIP.Unmap().AppendTo(buf[:0]))), nil
		}
		k, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		return b2v(!n.lim.Allow(k.s)), nil
	case opMatches:
		x, err := st.eval(n.kids[0])
		if err != nil {
			return value{}, err
		}
		return b2v(n.re.MatchString(x.s)), nil
	case opNow:
		return value{i: env.Now}, nil
	}
	return value{}, st.fail(n, "internal error: cannot evaluate node")
}

func cookie(h http.Header, name string) string {
	for _, line := range h["Cookie"] {
		for part := range strings.SplitSeq(line, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && k == name {
				return strings.Trim(v, "\"")
			}
		}
	}
	return ""
}
