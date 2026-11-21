package policy

// The syntax tree, as parsed. The checker (check.go) turns it into typed,
// resolved nodes for the interpreter.

type expr interface{ at() pos }

type (
	strLit struct {
		p pos
		v string
	}
	intLit struct {
		p pos
		v int64
	}
	boolLit struct {
		p pos
		v bool
	}
	listLit struct {
		p     pos
		elems []expr
	}
	ident struct { // path, method, host, client, response
		p    pos
		name string
	}
	selector struct { // client.ip, response.status
		p    pos
		x    expr
		name string
	}
	call struct { // f(args) or recv.f(args)
		p    pos
		recv expr // nil for a free function
		name string
		args []expr
	}
	unary struct {
		p  pos
		op string
		x  expr
	}
	binary struct {
		p    pos
		op   string
		x, y expr
	}
)

func (e *strLit) at() pos   { return e.p }
func (e *intLit) at() pos   { return e.p }
func (e *boolLit) at() pos  { return e.p }
func (e *listLit) at() pos  { return e.p }
func (e *ident) at() pos    { return e.p }
func (e *selector) at() pos { return e.p }
func (e *call) at() pos     { return e.p }
func (e *unary) at() pos    { return e.p }
func (e *binary) at() pos   { return e.p }

type stmt interface{ at() pos }

type (
	ifStmt struct {
		p         pos
		cond      expr
		then, els []stmt
	}
	callStmt struct{ c *call }
)

func (s *ifStmt) at() pos   { return s.p }
func (s *callStmt) at() pos { return s.c.p }

type script struct {
	name              string
	namePos           pos
	request, response []stmt
	hasReq, hasResp   bool
}

type parser struct {
	toks  []token
	i     int
	depth int
	nodes int
}

// parse builds the syntax tree, stopping at the first syntax error.
func parse(src string) (*script, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	s, err := p.script()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) tok() token { return p.toks[p.i] }

func (p *parser) is(kind tokKind, text string) bool {
	t := p.tok()
	return t.kind == kind && t.text == text
}

func (p *parser) punct(text string) bool { return p.is(tPunct, text) }

func (p *parser) expect(kind tokKind, text string) (token, error) {
	t := p.tok()
	if t.kind != kind || (text != "" && t.text != text) {
		want := text
		if want == "" {
			want = map[tokKind]string{tIdent: "identifier", tString: "string", tInt: "integer"}[kind]
		}
		return t, errAt(t.pos, "expected %s, found %s", quoteWant(want, kind), describe(t))
	}
	p.i++
	return t, nil
}

func quoteWant(w string, k tokKind) string {
	if k == tPunct || k == tIdent && w != "identifier" {
		return "\"" + w + "\""
	}
	return w
}

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "end of input"
	case tString:
		return "string"
	case tInt:
		return "integer"
	}
	return "\"" + t.text + "\""
}

// enter guards depth and size, so a hostile policy cannot blow the stack or memory.
func (p *parser) enter() error {
	p.depth++
	p.nodes++
	if p.depth > MaxDepth {
		return errAt(p.tok().pos, "nesting is deeper than %d", MaxDepth)
	}
	if p.nodes > MaxNodes {
		return errAt(p.tok().pos, "policy has more than %d syntax nodes", MaxNodes)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) script() (*script, error) {
	if _, err := p.expect(tIdent, "policy"); err != nil {
		return nil, err
	}
	name, err := p.expect(tString, "")
	if err != nil {
		return nil, err
	}
	if name.text == "" {
		return nil, errAt(name.pos, "policy name must not be empty")
	}
	s := &script{name: name.text, namePos: name.pos}
	if _, err := p.expect(tPunct, "{"); err != nil {
		return nil, err
	}
	for !p.punct("}") {
		on, err := p.expect(tIdent, "on")
		if err != nil {
			return nil, err
		}
		phase := p.tok()
		if phase.kind != tIdent || (phase.text != "request" && phase.text != "response") {
			return nil, errAt(phase.pos, "expected \"request\" or \"response\" after \"on\", found %s", describe(phase))
		}
		p.i++
		body, err := p.block()
		if err != nil {
			return nil, err
		}
		if phase.text == "request" {
			if s.hasReq {
				return nil, errAt(on.pos, "duplicate \"on request\" handler")
			}
			s.request, s.hasReq = body, true
		} else {
			if s.hasResp {
				return nil, errAt(on.pos, "duplicate \"on response\" handler")
			}
			s.response, s.hasResp = body, true
		}
	}
	p.i++
	if t := p.tok(); t.kind != tEOF {
		return nil, errAt(t.pos, "expected end of input after the policy, found %s (one policy per script)", describe(t))
	}
	return s, nil
}

func (p *parser) block() ([]stmt, error) {
	if _, err := p.expect(tPunct, "{"); err != nil {
		return nil, err
	}
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	var out []stmt
	for !p.punct("}") {
		if p.tok().kind == tEOF {
			return nil, errAt(p.tok().pos, "expected \"}\", found end of input")
		}
		s, err := p.stmt()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if p.punct(";") {
			p.i++
		}
	}
	p.i++
	return out, nil
}

func (p *parser) stmt() (stmt, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	t := p.tok()
	if t.kind == tIdent && t.text == "if" {
		p.i++
		cond, err := p.expr()
		if err != nil {
			return nil, err
		}
		then, err := p.block()
		if err != nil {
			return nil, err
		}
		s := &ifStmt{p: t.pos, cond: cond, then: then}
		if p.is(tIdent, "else") {
			p.i++
			if p.is(tIdent, "if") {
				nested, err := p.stmt()
				if err != nil {
					return nil, err
				}
				s.els = []stmt{nested}
			} else if s.els, err = p.block(); err != nil {
				return nil, err
			}
		}
		return s, nil
	}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	c, ok := e.(*call)
	if !ok {
		return nil, errAt(e.at(), "expected a statement (an action call or if), found an expression whose value is unused")
	}
	return &callStmt{c: c}, nil
}

func (p *parser) expr() (expr, error) { return p.binaryLevel(0) }

// Binary operators by level, loosest first. Comparisons (level 2) do not chain.
var levels = [][]string{{"||"}, {"&&"}, {"==", "!=", "<", "<=", ">", ">=", "in"}, {"+"}}

func (p *parser) binaryLevel(lvl int) (expr, error) {
	if lvl == len(levels) {
		return p.unary()
	}
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	x, err := p.binaryLevel(lvl + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.tok()
		op := ""
		for _, o := range levels[lvl] {
			if (t.kind == tPunct || t.kind == tIdent) && t.text == o {
				op = o
			}
		}
		if op == "" {
			return x, nil
		}
		p.i++
		y, err := p.binaryLevel(lvl + 1)
		if err != nil {
			return nil, err
		}
		x = &binary{p: t.pos, op: op, x: x, y: y}
		p.nodes++
		if lvl == 2 {
			if n := p.tok(); (n.kind == tPunct || n.kind == tIdent) && isCmp(n.text) {
				return nil, errAt(n.pos, "comparisons cannot be chained; use && and parentheses")
			}
			return x, nil
		}
	}
}

func isCmp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=", "in":
		return true
	}
	return false
}

func (p *parser) unary() (expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	if t := p.tok(); t.kind == tPunct && t.text == "!" {
		p.i++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unary{p: t.pos, op: "!", x: x}, nil
	}
	return p.postfix()
}

func (p *parser) postfix() (expr, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.punct(".") {
		dot := p.tok()
		p.i++
		name, err := p.expect(tIdent, "")
		if err != nil {
			return nil, err
		}
		if err := p.enter(); err != nil {
			return nil, err
		}
		p.leave()
		if p.punct("(") {
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			x = &call{p: name.pos, recv: x, name: name.text, args: args}
		} else {
			x = &selector{p: dot.pos, x: x, name: name.text}
		}
	}
	return x, nil
}

func (p *parser) args() ([]expr, error) {
	p.i++ // (
	var args []expr
	for !p.punct(")") {
		a, err := p.expr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.punct(",") {
			p.i++
		} else if !p.punct(")") {
			return nil, errAt(p.tok().pos, "expected \",\" or \")\", found %s", describe(p.tok()))
		}
	}
	p.i++
	return args, nil
}

func (p *parser) primary() (expr, error) {
	t := p.tok()
	switch t.kind {
	case tString:
		p.i++
		return &strLit{p: t.pos, v: t.text}, nil
	case tInt:
		p.i++
		return &intLit{p: t.pos, v: t.num}, nil
	case tIdent:
		switch t.text {
		case "true", "false":
			p.i++
			return &boolLit{p: t.pos, v: t.text == "true"}, nil
		case "policy", "on", "request", "response", "if", "else", "in":
			if t.text != "response" { // "response" is also a namespace: response.status
				return nil, errAt(t.pos, "unexpected keyword %q", t.text)
			}
		}
		p.i++
		if p.punct("(") {
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			return &call{p: t.pos, name: t.text, args: args}, nil
		}
		return &ident{p: t.pos, name: t.text}, nil
	case tPunct:
		switch t.text {
		case "(":
			p.i++
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tPunct, ")"); err != nil {
				return nil, err
			}
			return x, nil
		case "[":
			p.i++
			l := &listLit{p: t.pos}
			for !p.punct("]") {
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				l.elems = append(l.elems, e)
				if len(l.elems) > MaxListLen {
					return nil, errAt(t.pos, "list has more than %d elements", MaxListLen)
				}
				if p.punct(",") {
					p.i++
				} else if !p.punct("]") {
					return nil, errAt(p.tok().pos, "expected \",\" or \"]\", found %s", describe(p.tok()))
				}
			}
			p.i++
			return l, nil
		}
	}
	return nil, errAt(t.pos, "expected an expression, found %s", describe(t))
}
