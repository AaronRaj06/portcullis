package policy

import (
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/AaronRaj06/portcullis/internal/ratelimit"
)

type typ uint8

const (
	tyVoid typ = iota
	tyString
	tyInt
	tyBool
	tyIP
	tyListStr
	tyListInt
	tyBad // after an error, so one mistake is not reported again by every parent
)

func (t typ) String() string {
	return [...]string{"void", "string", "int", "bool", "ip", "list<string>", "list<int>", "invalid"}[t]
}

type opcode uint8

const (
	opStr opcode = iota
	opInt
	opBool
	opMethod
	opHost
	opPath
	opClientIP
	opStatus
	opHeader // kids[0] name; s = canonical name when it is a literal
	opQuery
	opCookie
	opRespHeader
	opStartsWith
	opEndsWith
	opContains
	opLower
	opUpper
	opLen
	opConcat
	opEq
	opNe
	opLt
	opLe
	opGt
	opGe
	opInStrs // kids[0] in strs
	opInInts // kids[0] in ints
	opInNets // kids[0] in prefixes
	opAnd
	opOr
	opNot
	opRateLimit
	opMatches // kids[0] matched against re
	opNow
	// statements
	opIf
	opDeny
	opAllow
	opRedirect
	opRouteTo
	opRewrite
	opSetHeader
	opRemoveHeader
	opLog
	opTag
)

// node is a checked, resolved syntax-tree node, evaluated by eval.go.
type node struct {
	op        opcode
	t         typ
	s         string
	i         int64
	kids      []*node
	then, els []*node
	strs      []string
	ints      []int64
	nets      []netip.Prefix
	lim       *ratelimit.Limiter
	re        *regexp.Regexp
	p         pos
}

// Headers a policy may not set or remove: changing them could desynchronise
// how the proxy and the backend frame the request (request smuggling).
var protectedHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true, "Upgrade": true,
	"Te": true, "Trailer": true, "Keep-Alive": true, "Proxy-Connection": true, "X-Request-Id": true,
}

type checker struct {
	errs     ErrorList
	response bool // checking the response handler
	name     string
	o        Options
	pools    map[string]bool
}

func (c *checker) errorf(p pos, format string, a ...any) {
	c.errs = append(c.errs, errAt(p, format, a...))
}

func (c *checker) stmts(ss []stmt) []*node {
	out := make([]*node, 0, len(ss))
	for _, s := range ss {
		if n := c.stmt(s); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (c *checker) stmt(s stmt) *node {
	switch s := s.(type) {
	case *ifStmt:
		cond := c.expr(s.cond)
		if cond.t != tyBool && cond.t != tyBad {
			c.errorf(s.cond.at(), "if condition must be bool, found %s", cond.t)
		}
		return &node{op: opIf, p: s.p, kids: []*node{cond}, then: c.stmts(s.then), els: c.stmts(s.els)}
	case *callStmt:
		if s.c.recv == nil {
			if n, ok := c.action(s.c); ok {
				return n
			}
		}
		n := c.expr(s.c)
		if n.t != tyBad {
			c.errorf(s.c.p, "the value of %s(...) is unused; a statement must be an action such as deny(...) or set_header(...)", s.c.name)
		}
		return nil
	}
	return nil
}

func (c *checker) args(cl *call, n int, opt int) bool {
	if len(cl.args) < n || len(cl.args) > n+opt {
		want := fmt.Sprint(n)
		if opt > 0 {
			want = fmt.Sprintf("%d or %d", n, n+opt)
		}
		c.errorf(cl.p, "%s takes %s argument(s), found %d", cl.name, want, len(cl.args))
		return false
	}
	return true
}

func (c *checker) want(e expr, t typ) *node {
	n := c.expr(e)
	if n.t != t && n.t != tyBad {
		c.errorf(e.at(), "expected %s, found %s", t, n.t)
		n.t = tyBad
	}
	return n
}

func (c *checker) intLit(e expr, what string) (int64, bool) {
	if l, ok := e.(*intLit); ok {
		return l.v, true
	}
	c.errorf(e.at(), "%s must be an integer literal", what)
	return 0, false
}

func (c *checker) strLit(e expr, what string) (string, bool) {
	if l, ok := e.(*strLit); ok {
		return l.v, true
	}
	c.errorf(e.at(), "%s must be a string literal", what)
	return "", false
}

func (c *checker) headerName(e expr) string {
	name, ok := c.strLit(e, "header name")
	if !ok {
		return ""
	}
	if !httpguts.ValidHeaderFieldName(name) {
		c.errorf(e.at(), "%q is not a valid header name", name)
		return ""
	}
	canon := http.CanonicalHeaderKey(name)
	if protectedHeaders[canon] {
		c.errorf(e.at(), "header %s cannot be changed by a policy (it controls request framing)", canon)
	}
	return canon
}

func (c *checker) requestOnly(cl *call) bool {
	if c.response {
		c.errorf(cl.p, "%s is only allowed in \"on request\"", cl.name)
		return false
	}
	return true
}

// action checks a statement-level call. ok is false if name is not an action.
func (c *checker) action(cl *call) (*node, bool) {
	n := &node{p: cl.p, t: tyVoid}
	switch cl.name {
	case "deny":
		n.op = opDeny
		if c.requestOnly(cl) && c.args(cl, 1, 1) {
			if v, ok := c.intLit(cl.args[0], "deny status"); ok {
				if v < 400 || v > 599 {
					c.errorf(cl.args[0].at(), "deny status %d is outside 400..599", v)
				}
				n.i = v
			}
			if len(cl.args) == 2 {
				n.kids = []*node{c.want(cl.args[1], tyString)}
			}
		}
	case "allow":
		n.op = opAllow
		if c.requestOnly(cl) {
			c.args(cl, 0, 0)
		}
	case "redirect":
		n.op, n.i = opRedirect, http.StatusFound
		if c.requestOnly(cl) && c.args(cl, 1, 1) {
			n.kids = []*node{c.want(cl.args[0], tyString)}
			if len(cl.args) == 2 {
				if v, ok := c.intLit(cl.args[1], "redirect status"); ok {
					switch v {
					case 301, 302, 303, 307, 308:
						n.i = v
					default:
						c.errorf(cl.args[1].at(), "redirect status %d is not 301, 302, 303, 307 or 308", v)
					}
				}
			}
		}
	case "route_to":
		n.op = opRouteTo
		if c.requestOnly(cl) && c.args(cl, 1, 0) {
			if v, ok := c.strLit(cl.args[0], "route_to pool"); ok {
				if c.pools != nil && !c.pools[v] {
					c.errorf(cl.args[0].at(), "unknown pool %q", v)
				}
				n.s = v
			}
		}
	case "rewrite_path":
		n.op = opRewrite
		if c.requestOnly(cl) && c.args(cl, 1, 0) {
			n.kids = []*node{c.want(cl.args[0], tyString)}
			if l, ok := cl.args[0].(*strLit); ok && !strings.HasPrefix(l.v, "/") {
				c.errorf(l.p, "rewrite_path %q must start with /", l.v)
			}
		}
	case "set_header":
		n.op = opSetHeader
		if c.args(cl, 2, 0) {
			n.s = c.headerName(cl.args[0])
			n.kids = []*node{c.want(cl.args[1], tyString)}
		}
	case "remove_header":
		n.op = opRemoveHeader
		if c.args(cl, 1, 0) {
			n.s = c.headerName(cl.args[0])
		}
	case "log":
		n.op = opLog
		if c.args(cl, 1, 0) {
			n.kids = []*node{c.want(cl.args[0], tyString)}
		}
	case "tag":
		n.op = opTag
		if c.args(cl, 1, 0) {
			n.s, _ = c.strLit(cl.args[0], "tag")
		}
	default:
		return nil, false
	}
	return n, true
}

var actions = map[string]bool{"deny": true, "allow": true, "redirect": true, "route_to": true, "rewrite_path": true,
	"set_header": true, "remove_header": true, "log": true, "tag": true}

func bad(p pos) *node { return &node{t: tyBad, p: p} }

func (c *checker) expr(e expr) *node {
	switch e := e.(type) {
	case *strLit:
		return &node{op: opStr, t: tyString, s: e.v, p: e.p}
	case *intLit:
		return &node{op: opInt, t: tyInt, i: e.v, p: e.p}
	case *boolLit:
		n := &node{op: opBool, t: tyBool, p: e.p}
		if e.v {
			n.i = 1
		}
		return n
	case *listLit:
		c.errorf(e.p, "a list can only appear on the right of \"in\"")
		return bad(e.p)
	case *ident:
		switch e.name {
		case "method":
			return &node{op: opMethod, t: tyString, p: e.p}
		case "host":
			return &node{op: opHost, t: tyString, p: e.p}
		case "path":
			return &node{op: opPath, t: tyString, p: e.p}
		case "client":
			c.errorf(e.p, "client is a namespace; use client.ip")
		case "response":
			c.errorf(e.p, "response is a namespace; use response.status or response.header(name)")
		default:
			c.errorf(e.p, "unknown name %q", e.name)
		}
		return bad(e.p)
	case *selector:
		if id, ok := e.x.(*ident); ok {
			switch {
			case id.name == "client" && e.name == "ip":
				return &node{op: opClientIP, t: tyIP, p: e.p}
			case id.name == "response" && e.name == "status":
				if !c.response {
					c.errorf(e.p, "response.status is only available in \"on response\"")
					return bad(e.p)
				}
				return &node{op: opStatus, t: tyInt, p: e.p}
			}
		}
		c.errorf(e.p, "unknown field %q", e.name)
		return bad(e.p)
	case *unary:
		x := c.want(e.x, tyBool)
		return &node{op: opNot, t: tyBool, kids: []*node{x}, p: e.p}
	case *binary:
		return c.binary(e)
	case *call:
		return c.call(e)
	}
	panic(fmt.Sprintf("policy: unknown expression %T", e))
}

func (c *checker) binary(e *binary) *node {
	if e.op == "in" {
		return c.in(e)
	}
	x, y := c.expr(e.x), c.expr(e.y)
	n := &node{t: tyBool, kids: []*node{x, y}, p: e.p}
	if x.t == tyBad || y.t == tyBad {
		n.t = tyBad
		return n
	}
	switch e.op {
	case "&&", "||":
		if x.t != tyBool || y.t != tyBool {
			c.errorf(e.p, "%s needs bool operands, found %s and %s", e.op, x.t, y.t)
		}
		n.op = map[string]opcode{"&&": opAnd, "||": opOr}[e.op]
	case "+":
		if x.t != tyString || y.t != tyString {
			c.errorf(e.p, "+ joins strings, found %s and %s", x.t, y.t)
		}
		n.op, n.t = opConcat, tyString
	case "==", "!=":
		if x.t != y.t || (x.t != tyString && x.t != tyInt && x.t != tyBool) {
			c.errorf(e.p, "cannot compare %s %s %s (both sides must be string, int or bool, and the same)", x.t, e.op, y.t)
		}
		n.op = map[string]opcode{"==": opEq, "!=": opNe}[e.op]
	default:
		if x.t != tyInt || y.t != tyInt {
			c.errorf(e.p, "%s needs int operands, found %s and %s", e.op, x.t, y.t)
		}
		n.op = map[string]opcode{"<": opLt, "<=": opLe, ">": opGt, ">=": opGe}[e.op]
	}
	return n
}

func (c *checker) in(e *binary) *node {
	x := c.expr(e.x)
	n := &node{t: tyBool, kids: []*node{x}, p: e.p}
	list, ok := e.y.(*listLit)
	if !ok {
		c.errorf(e.y.at(), "the right side of \"in\" must be a list literal like [\"a\", \"b\"]")
		n.t = tyBad
		return n
	}
	for _, el := range list.elems {
		switch v := el.(type) {
		case *strLit:
			switch x.t {
			case tyIP:
				pfx, err := parseNet(v.v)
				if err != nil {
					c.errorf(v.p, "%q is not an IP address or CIDR", v.v)
				}
				n.nets = append(n.nets, pfx)
			case tyString:
				n.strs = append(n.strs, v.v)
			case tyBad:
			default:
				c.errorf(v.p, "cannot look for %s in a list of strings", x.t)
				return bad(e.p)
			}
		case *intLit:
			if x.t != tyInt && x.t != tyBad {
				c.errorf(v.p, "cannot look for %s in a list of ints", x.t)
				return bad(e.p)
			}
			n.ints = append(n.ints, v.v)
		default:
			c.errorf(el.at(), "list elements must be string or integer literals")
			return bad(e.p)
		}
	}
	if x.t == tyBad {
		n.t = tyBad
	}
	switch x.t {
	case tyIP:
		n.op = opInNets
	case tyInt:
		n.op = opInInts
	default:
		n.op = opInStrs
	}
	return n
}

func parseNet(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

func (c *checker) call(cl *call) *node {
	if cl.recv != nil {
		return c.method(cl)
	}
	if actions[cl.name] {
		c.errorf(cl.p, "%s is an action and has no value; use it as a statement", cl.name)
		return bad(cl.p)
	}
	n := &node{p: cl.p, t: tyString}
	switch cl.name {
	case "header", "query", "cookie":
		n.op = map[string]opcode{"header": opHeader, "query": opQuery, "cookie": opCookie}[cl.name]
		if !c.args(cl, 1, 0) {
			return bad(cl.p)
		}
		n.kids = []*node{c.want(cl.args[0], tyString)}
		if l, ok := cl.args[0].(*strLit); ok && cl.name == "header" {
			n.s = http.CanonicalHeaderKey(l.v)
		}
		return n
	case "rate_limit":
		return c.rateLimit(cl)
	case "now":
		if !c.args(cl, 0, 0) {
			return bad(cl.p)
		}
		return &node{op: opNow, t: tyInt, p: cl.p}
	}
	c.errorf(cl.p, "unknown function %q", cl.name)
	return bad(cl.p)
}

func (c *checker) rateLimit(cl *call) *node {
	if !c.requestOnly(cl) || !c.args(cl, 3, 1) {
		return bad(cl.p)
	}
	key := c.expr(cl.args[0])
	if key.t != tyString && key.t != tyIP && key.t != tyBad {
		c.errorf(cl.args[0].at(), "rate_limit key must be a string or ip, found %s", key.t)
	}
	limit, ok1 := c.intLit(cl.args[1], "rate_limit limit")
	if ok1 && limit < 1 {
		c.errorf(cl.args[1].at(), "rate_limit limit must be at least 1")
		ok1 = false
	}
	ws, ok2 := c.strLit(cl.args[2], "rate_limit window")
	var window time.Duration
	if ok2 {
		var err error
		window, err = time.ParseDuration(ws)
		if err != nil || window < time.Second || window > 24*time.Hour {
			c.errorf(cl.args[2].at(), "rate_limit window %q must be a duration from \"1s\" to \"24h\"", ws)
			ok2 = false
		}
	}
	// The call site names the limiter: by position, or by an explicit name so
	// editing the script elsewhere does not move it (and reset its counters).
	site := cl.p.String()
	if len(cl.args) == 4 {
		name, ok := c.strLit(cl.args[3], "rate_limit name")
		if ok && (name == "" || len(name) > 64) {
			c.errorf(cl.args[3].at(), "rate_limit name must be 1 to 64 characters")
			ok = false
		}
		ok2 = ok2 && ok
		site = "name:" + name
	}
	n := &node{op: opRateLimit, t: tyBool, kids: []*node{key}, p: cl.p}
	if ok1 && ok2 {
		// Each of Share replicas enforces its part, so together they approximate
		// the global limit when the load balancer spreads clients evenly.
		share := max(1, int64(c.o.Share))
		limit = max(1, (limit+share-1)/share)
		n.lim = c.o.limiter(fmt.Sprintf("%s@%s/%d/%s", c.name, site, limit, window), int(limit), window)
	}
	return n
}

func (c *checker) method(cl *call) *node {
	if id, ok := cl.recv.(*ident); ok && id.name == "response" && cl.name == "header" {
		if !c.response {
			c.errorf(cl.p, "response.header is only available in \"on response\"")
			return bad(cl.p)
		}
		if !c.args(cl, 1, 0) {
			return bad(cl.p)
		}
		n := &node{op: opRespHeader, t: tyString, kids: []*node{c.want(cl.args[0], tyString)}, p: cl.p}
		if l, ok := cl.args[0].(*strLit); ok {
			n.s = http.CanonicalHeaderKey(l.v)
		}
		return n
	}
	recv := c.expr(cl.recv)
	if recv.t != tyString && recv.t != tyBad {
		c.errorf(cl.p, "%s has no method %q", recv.t, cl.name)
		return bad(cl.p)
	}
	n := &node{p: cl.p, kids: []*node{recv}}
	switch cl.name {
	case "starts_with", "ends_with", "contains":
		n.op = map[string]opcode{"starts_with": opStartsWith, "ends_with": opEndsWith, "contains": opContains}[cl.name]
		n.t = tyBool
		if !c.args(cl, 1, 0) {
			return bad(cl.p)
		}
		n.kids = append(n.kids, c.want(cl.args[0], tyString))
	case "lower", "upper":
		n.op = map[string]opcode{"lower": opLower, "upper": opUpper}[cl.name]
		n.t = tyString
		c.args(cl, 0, 0)
	case "len":
		n.op, n.t = opLen, tyInt
		c.args(cl, 0, 0)
	case "matches":
		n.op, n.t = opMatches, tyBool
		if !c.args(cl, 1, 0) {
			return bad(cl.p)
		}
		pat, ok := c.strLit(cl.args[0], "matches pattern")
		if !ok {
			return bad(cl.p)
		}
		re, err := regexp.Compile(pat) // RE2: matching time is linear in the input, whatever the pattern
		if err != nil {
			c.errorf(cl.args[0].at(), "invalid regular expression: %v", err)
			return bad(cl.p)
		}
		n.re = re
	default:
		c.errorf(cl.p, "string has no method %q", cl.name)
		return bad(cl.p)
	}
	if recv.t == tyBad {
		n.t = tyBad
	}
	return n
}
