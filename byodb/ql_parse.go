package byodb

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// QLNode is an expression tree node. Literals reuse Value with Type set to
// TYPE_INT64 or TYPE_BYTES; operators use the QL_* codes below.
type QLNode struct {
	Value
	Kids []QLNode
}

const (
	QL_UNINIT = 0
	// literals: TYPE_BYTES = 1, TYPE_INT64 = 2
	QL_SYM    = 10 // column name
	QL_TUP    = 11 // (a, b, ...)
	QL_STAR   = 12 // select *
	QL_NEG    = 20 // -a
	QL_NOT    = 21
	QL_ADD    = 30
	QL_SUB    = 31
	QL_MUL    = 32
	QL_DIV    = 33
	QL_MOD    = 34
	QL_CMP_EQ = 40
	QL_CMP_NE = 41
	QL_CMP_LT = 42
	QL_CMP_LE = 43
	QL_CMP_GT = 44
	QL_CMP_GE = 45
	QL_AND    = 50
	QL_OR     = 51
)

// ---- statements ----

// QLScan is the part shared by SELECT, UPDATE and DELETE:
//
//	INDEX BY <range>  picks an index and a key range (and the sort order)
//	FILTER <expr>     filters rows after they are fetched
//	LIMIT [offset,] count
type QLScan struct {
	Table  string
	Key1   QLNode
	Key2   QLNode
	Filter QLNode
	Offset int64
	Limit  int64
}

type QLSelect struct {
	QLScan
	Names  []string
	Output []QLNode
}

type QLInsert struct {
	Table  string
	Mode   int // MODE_INSERT_ONLY or MODE_UPSERT
	Names  []string
	Values [][]QLNode
}

type QLUpdate struct {
	QLScan
	Names  []string
	Values []QLNode
}

type QLDelete struct{ QLScan }
type QLCreateTable struct{ Def TableDef }
type QLDropTable struct{ Name string }
type QLBegin struct{}
type QLCommit struct{}
type QLRollback struct{}
type QLShowTables struct{}
type QLDescribe struct{ Table string }

// ---- tokenizer ----

const (
	tokEOF = iota
	tokIdent
	tokQuotedIdent
	tokInt
	tokStr
	tokPunct
)

type token struct {
	kind  int
	text  string // identifier / punctuation text
	str   []byte // string literal
	i64   int64
	start int
	end   int
}

type syntaxError struct {
	msg string
	pos int
}

func (e *syntaxError) Error() string { return e.msg }

func isIdentStart(ch byte) bool {
	return ch == '_' || ('a' <= ch && ch <= 'z') || ('A' <= ch && ch <= 'Z')
}

func isIdentChar(ch byte) bool { return isIdentStart(ch) || ('0' <= ch && ch <= '9') }

var puncts = []string{"<=", ">=", "!=", "<>", "==", "(", ")", ",", ";", "*", "+", "-", "/", "%", "=", "<", ">"}

func tokenize(src string) ([]token, error) {
	var toks []token
	i := 0
	for {
		// skip whitespace and -- comments
		for i < len(src) {
			if strings.ContainsRune(" \t\r\n", rune(src[i])) {
				i++
			} else if strings.HasPrefix(src[i:], "--") {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			} else {
				break
			}
		}
		if i >= len(src) {
			toks = append(toks, token{kind: tokEOF, start: i, end: i})
			return toks, nil
		}
		start := i
		ch := src[i]
		switch {
		case isIdentStart(ch):
			for i < len(src) && isIdentChar(src[i]) {
				i++
			}
			toks = append(toks, token{kind: tokIdent, text: src[start:i]})
		case '0' <= ch && ch <= '9':
			for i < len(src) && '0' <= src[i] && src[i] <= '9' {
				i++
			}
			if i < len(src) && isIdentStart(src[i]) {
				return nil, &syntaxError{"invalid number", start}
			}
			n, err := strconv.ParseInt(src[start:i], 10, 64)
			if err != nil {
				// allow -9223372036854775808 via the unary minus
				if src[start:i] != "9223372036854775808" {
					return nil, &syntaxError{"integer out of range", start}
				}
				n = math.MinInt64
			}
			toks = append(toks, token{kind: tokInt, i64: n, text: src[start:i]})
		case ch == '\'' || ch == '"' || ch == '`':
			quote := ch
			var buf []byte
			i++
			for {
				if i >= len(src) {
					return nil, &syntaxError{"unterminated quote", start}
				}
				c := src[i]
				i++
				if c == quote {
					if i < len(src) && src[i] == quote { // doubled quote
						buf = append(buf, quote)
						i++
						continue
					}
					break
				}
				if c == '\\' && quote != '`' && i < len(src) {
					esc := src[i]
					i++
					switch esc {
					case 'n':
						c = '\n'
					case 't':
						c = '\t'
					case 'r':
						c = '\r'
					case '0':
						c = 0
					case 'x':
						if i+2 > len(src) {
							return nil, &syntaxError{"bad \\x escape", i}
						}
						v, err := strconv.ParseUint(src[i:i+2], 16, 8)
						if err != nil {
							return nil, &syntaxError{"bad \\x escape", i}
						}
						c = byte(v)
						i += 2
					default:
						c = esc
					}
				}
				buf = append(buf, c)
			}
			if quote == '`' {
				toks = append(toks, token{kind: tokQuotedIdent, text: string(buf)})
			} else {
				toks = append(toks, token{kind: tokStr, str: buf})
			}
		default:
			matched := ""
			for _, p := range puncts {
				if strings.HasPrefix(src[i:], p) {
					matched = p
					break
				}
			}
			if matched == "" {
				return nil, &syntaxError{fmt.Sprintf("unexpected character %q", ch), start}
			}
			i += len(matched)
			toks = append(toks, token{kind: tokPunct, text: matched})
		}
		toks[len(toks)-1].start, toks[len(toks)-1].end = start, i
	}
}

// StatementComplete reports whether src ends with a complete statement
// (terminated by `;` outside quotes and comments). Used by the shell.
func StatementComplete(src string) bool {
	toks, err := tokenize(src)
	if err != nil {
		if se, ok := err.(*syntaxError); ok && se.msg == "unterminated quote" {
			return false
		}
		return true // let the parser report it
	}
	return len(toks) >= 2 && toks[len(toks)-2].kind == tokPunct && toks[len(toks)-2].text == ";"
}

// ---- parser ----

type Parser struct {
	src  string
	toks []token
	i    int
}

// ParseSQL parses a sequence of `;`-separated statements.
func ParseSQL(src string) (stmts []interface{}, err error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, locate(src, err.(*syntaxError))
	}
	p := &Parser{src: src, toks: toks}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*syntaxError)
			if !ok {
				panic(r)
			}
			stmts, err = nil, locate(src, se)
		}
	}()
	for {
		for p.punct(";") {
		}
		if p.peek().kind == tokEOF {
			return stmts, nil
		}
		stmts = append(stmts, pStmt(p))
		if p.peek().kind != tokEOF {
			p.expectPunct(";")
		}
	}
}

func locate(src string, e *syntaxError) error {
	line, col := 1, 1
	for _, ch := range src[:min(e.pos, len(src))] {
		if ch == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Errorf("syntax error at line %d, column %d: %s", line, col, e.msg)
}

func (p *Parser) peek() token { return p.toks[p.i] }

func (p *Parser) fail(format string, args ...interface{}) {
	tok := p.peek()
	near := "end of input"
	if tok.kind != tokEOF {
		near = fmt.Sprintf("%q", p.src[tok.start:tok.end])
	}
	panic(&syntaxError{fmt.Sprintf(format, args...) + ", near " + near, tok.start})
}

// isKeyword checks (without consuming) for a sequence of keywords.
func (p *Parser) isKeyword(words ...string) bool {
	for j, w := range words {
		tok := p.toks[min(p.i+j, len(p.toks)-1)]
		if tok.kind != tokIdent || !strings.EqualFold(tok.text, w) {
			return false
		}
	}
	return true
}

func (p *Parser) keyword(words ...string) bool {
	if p.isKeyword(words...) {
		p.i += len(words)
		return true
	}
	return false
}

func (p *Parser) expectKeyword(words ...string) {
	if !p.keyword(words...) {
		p.fail("expected %s", strings.ToUpper(strings.Join(words, " ")))
	}
}

func (p *Parser) punct(s string) bool {
	if tok := p.peek(); tok.kind == tokPunct && tok.text == s {
		p.i++
		return true
	}
	return false
}

func (p *Parser) expectPunct(s string) {
	if !p.punct(s) {
		p.fail("expected `%s`", s)
	}
}

var reserved = map[string]bool{
	"select": true, "from": true, "index": true, "by": true, "filter": true,
	"limit": true, "and": true, "or": true, "not": true, "as": true,
	"insert": true, "upsert": true, "into": true, "values": true, "update": true,
	"set": true, "delete": true, "create": true, "drop": true, "table": true,
	"primary": true, "key": true,
}

// name parses an identifier (a column or table name).
func (p *Parser) name(what string) string {
	tok := p.peek()
	switch {
	case tok.kind == tokQuotedIdent:
	case tok.kind == tokIdent && !reserved[strings.ToLower(tok.text)]:
	default:
		p.fail("expected %s", what)
	}
	p.i++
	return tok.text
}

func (p *Parser) nameList(what string) []string {
	p.expectPunct("(")
	names := []string{p.name(what)}
	for p.punct(",") {
		names = append(names, p.name(what))
	}
	p.expectPunct(")")
	return names
}

func pStmt(p *Parser) interface{} {
	switch {
	case p.keyword("create", "table"):
		return pCreateTable(p)
	case p.keyword("drop", "table"):
		return &QLDropTable{Name: p.name("table name")}
	case p.keyword("select"):
		return pSelect(p)
	case p.keyword("insert", "into"):
		return pInsert(p, MODE_INSERT_ONLY)
	case p.keyword("upsert", "into"):
		return pInsert(p, MODE_UPSERT)
	case p.keyword("update"):
		return pUpdate(p)
	case p.keyword("delete", "from"):
		stmt := &QLDelete{}
		stmt.Table = p.name("table name")
		pScan(p, &stmt.QLScan)
		return stmt
	case p.keyword("begin"):
		p.keyword("transaction")
		return &QLBegin{}
	case p.keyword("commit"):
		return &QLCommit{}
	case p.keyword("rollback"), p.keyword("abort"):
		return &QLRollback{}
	case p.keyword("show", "tables"):
		return &QLShowTables{}
	case p.keyword("describe"):
		return &QLDescribe{Table: p.name("table name")}
	default:
		p.fail("expected a statement")
		return nil
	}
}

func pCreateTable(p *Parser) *QLCreateTable {
	stmt := &QLCreateTable{}
	def := &stmt.Def
	def.Name = p.name("table name")
	def.Indexes = [][]string{nil} // slot 0 is the primary key
	p.expectPunct("(")
	for {
		switch {
		case p.keyword("primary", "key"):
			if def.Indexes[0] != nil {
				p.fail("multiple primary keys")
			}
			def.Indexes[0] = p.nameList("column name")
		case p.keyword("index"):
			def.Indexes = append(def.Indexes, p.nameList("column name"))
		default:
			def.Cols = append(def.Cols, p.name("column name"))
			tok := p.peek()
			switch t := strings.ToLower(tok.text); {
			case tok.kind != tokIdent:
				p.fail("expected a column type")
			case t == "int" || t == "int64" || t == "integer" || t == "bigint":
				def.Types = append(def.Types, TYPE_INT64)
			case t == "string" || t == "bytes" || t == "text" || t == "varchar" || t == "blob":
				def.Types = append(def.Types, TYPE_BYTES)
			default:
				p.fail("unknown type (use int or string)")
			}
			p.i++
		}
		if !p.punct(",") {
			break
		}
		if p.peek().kind == tokPunct && p.peek().text == ")" {
			break // trailing comma
		}
	}
	p.expectPunct(")")
	if def.Indexes[0] == nil {
		p.fail("missing PRIMARY KEY")
	}
	return stmt
}

func pSelect(p *Parser) *QLSelect {
	stmt := &QLSelect{}
	for {
		start := p.i
		if p.punct("*") {
			stmt.Output = append(stmt.Output, QLNode{Value: Value{Type: QL_STAR}})
			stmt.Names = append(stmt.Names, "*")
		} else {
			node := pExprOr(p)
			name := ""
			if p.keyword("as") {
				name = p.name("output name")
			} else if node.Type == QL_SYM {
				name = string(node.Str)
			} else {
				name = p.src[p.toks[start].start:p.toks[p.i-1].end]
			}
			stmt.Output = append(stmt.Output, node)
			stmt.Names = append(stmt.Names, name)
		}
		if !p.punct(",") {
			break
		}
	}
	p.expectKeyword("from")
	stmt.Table = p.name("table name")
	pScan(p, &stmt.QLScan)
	return stmt
}

func pInsert(p *Parser, mode int) *QLInsert {
	stmt := &QLInsert{Mode: mode, Table: p.name("table name")}
	if p.peek().kind == tokPunct && p.peek().text == "(" {
		stmt.Names = p.nameList("column name")
	}
	p.expectKeyword("values")
	for {
		p.expectPunct("(")
		row := []QLNode{pExprOr(p)}
		for p.punct(",") {
			row = append(row, pExprOr(p))
		}
		p.expectPunct(")")
		stmt.Values = append(stmt.Values, row)
		if !p.punct(",") {
			break
		}
	}
	return stmt
}

func pUpdate(p *Parser) *QLUpdate {
	stmt := &QLUpdate{}
	stmt.Table = p.name("table name")
	p.expectKeyword("set")
	for {
		stmt.Names = append(stmt.Names, p.name("column name"))
		p.expectPunct("=")
		stmt.Values = append(stmt.Values, pExprOr(p))
		if !p.punct(",") {
			break
		}
	}
	pScan(p, &stmt.QLScan)
	return stmt
}

func pScan(p *Parser, scan *QLScan) {
	if p.keyword("index", "by") {
		scan.Key1 = pIndexKey(p)
		if p.keyword("and") {
			scan.Key2 = pIndexKey(p)
		}
	}
	if p.keyword("filter") {
		scan.Filter = pExprOr(p)
	}
	scan.Offset, scan.Limit = 0, math.MaxInt64
	if p.keyword("limit") {
		n := pLimitNum(p)
		if p.punct(",") {
			scan.Offset, scan.Limit = n, pLimitNum(p)
		} else {
			scan.Limit = n
		}
	}
}

func pIndexKey(p *Parser) QLNode {
	node := pExprCmp(p)
	if node.Type < QL_CMP_EQ || node.Type > QL_CMP_GE {
		p.fail("INDEX BY expects a comparison like `a > 1`")
	}
	return node
}

func pLimitNum(p *Parser) int64 {
	tok := p.peek()
	if tok.kind != tokInt || tok.i64 < 0 {
		p.fail("expected a non-negative integer")
	}
	p.i++
	return tok.i64
}

// ---- expressions, by precedence from lowest to highest ----
//
//	or  := and ('OR' and)*
//	and := not ('AND' not)*
//	not := 'NOT' not | cmp
//	cmp := add (cmpop add)?
//	add := mul (('+' | '-') mul)*
//	mul := unop (('*' | '/' | '%') unop)*
//	unop := '-' unop | atom
//	atom := number | string | name | '(' or (',' or)* ')'

func binop(op uint32, left, right QLNode) QLNode {
	return QLNode{Value: Value{Type: op}, Kids: []QLNode{left, right}}
}

func pExprOr(p *Parser) QLNode {
	node := pExprAnd(p)
	for p.keyword("or") {
		node = binop(QL_OR, node, pExprAnd(p))
	}
	return node
}

func pExprAnd(p *Parser) QLNode {
	node := pExprNot(p)
	for p.keyword("and") {
		node = binop(QL_AND, node, pExprNot(p))
	}
	return node
}

func pExprNot(p *Parser) QLNode {
	if p.keyword("not") {
		return QLNode{Value: Value{Type: QL_NOT}, Kids: []QLNode{pExprNot(p)}}
	}
	return pExprCmp(p)
}

var cmpOps = map[string]uint32{
	"=": QL_CMP_EQ, "==": QL_CMP_EQ, "!=": QL_CMP_NE, "<>": QL_CMP_NE,
	"<": QL_CMP_LT, "<=": QL_CMP_LE, ">": QL_CMP_GT, ">=": QL_CMP_GE,
}

func pExprCmp(p *Parser) QLNode {
	node := pExprAdd(p)
	if tok := p.peek(); tok.kind == tokPunct {
		if op, ok := cmpOps[tok.text]; ok {
			p.i++
			node = binop(op, node, pExprAdd(p))
		}
	}
	return node
}

func pExprAdd(p *Parser) QLNode {
	node := pExprMul(p)
	for {
		switch {
		case p.punct("+"):
			node = binop(QL_ADD, node, pExprMul(p))
		case p.punct("-"):
			node = binop(QL_SUB, node, pExprMul(p))
		default:
			return node
		}
	}
}

func pExprMul(p *Parser) QLNode {
	node := pExprUnop(p)
	for {
		switch {
		case p.punct("*"):
			node = binop(QL_MUL, node, pExprUnop(p))
		case p.punct("/"):
			node = binop(QL_DIV, node, pExprUnop(p))
		case p.punct("%"):
			node = binop(QL_MOD, node, pExprUnop(p))
		default:
			return node
		}
	}
}

func pExprUnop(p *Parser) QLNode {
	if p.punct("-") {
		kid := pExprUnop(p)
		if kid.Type == TYPE_INT64 { // fold negative literals
			kid.I64 = -kid.I64
			return kid
		}
		return QLNode{Value: Value{Type: QL_NEG}, Kids: []QLNode{kid}}
	}
	return pExprAtom(p)
}

func pExprAtom(p *Parser) QLNode {
	tok := p.peek()
	switch {
	case tok.kind == tokInt:
		p.i++
		if tok.i64 == math.MinInt64 && !(p.i >= 2 && p.toks[p.i-2].text == "-") {
			p.i--
			p.fail("integer out of range")
		}
		return QLNode{Value: Value{Type: TYPE_INT64, I64: tok.i64}}
	case tok.kind == tokStr:
		p.i++
		return QLNode{Value: Value{Type: TYPE_BYTES, Str: tok.str}}
	case tok.kind == tokPunct && tok.text == "(":
		p.i++
		kids := []QLNode{pExprOr(p)}
		for p.punct(",") {
			kids = append(kids, pExprOr(p))
		}
		p.expectPunct(")")
		if len(kids) == 1 {
			return kids[0]
		}
		return QLNode{Value: Value{Type: QL_TUP}, Kids: kids}
	case tok.kind == tokIdent && reserved[strings.ToLower(tok.text)]:
		p.fail("expected an expression")
	case tok.kind == tokIdent || tok.kind == tokQuotedIdent:
		p.i++
		return QLNode{Value: Value{Type: QL_SYM, Str: []byte(tok.text)}}
	}
	p.fail("expected an expression")
	return QLNode{}
}
