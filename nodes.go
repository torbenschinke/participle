package participle

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

var (
	// MaxIterations limits the number of elements capturable by {}.
	MaxIterations = 1000000

	positionType        = reflect.TypeOf(lexer.Position{})
	tokenType           = reflect.TypeOf(lexer.Token{})
	tokensType          = reflect.TypeOf([]lexer.Token{})
	captureType         = reflect.TypeOf((*Capture)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	parseableType       = reflect.TypeOf((*Parseable)(nil)).Elem()

	// NextMatch should be returned by Parseable.Parse() method implementations to indicate
	// that the node did not match and that other matches should be attempted, if appropriate.
	NextMatch = errors.New("no match") // nolint: golint
)

// A node in the grammar.
type node interface {
	// Parse from scanner into value.
	//
	// Returned slice will be nil if the node does not match.
	Parse(ctx *parseContext, parent reflect.Value) ([]reflect.Value, error)

	// Return a decent string representation of the Node.
	String() string
}

func nearestPos(tokens []lexer.Token) *lexer.Position {
	if len(tokens) == 0 {
		return nil
	}

	return &tokens[0].Pos
}

func decorate(err *error, pos *lexer.Position, name func() string) {
	if *err == nil {
		return
	}

	if pos == nil {
		*err = fmt.Errorf("%s: %w", name(), *err)
		return
	}

	*err = Wrapf(*pos, *err, "%s", name())
}

// A node that proxies to an implementation that implements the Parseable interface.
type parseable struct {
	t reflect.Type
}

func (p *parseable) String() string { return ebnf(p) }

func (p *parseable) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	rv := reflect.New(p.t)
	v := rv.Interface().(Parseable)
	err = v.Parse(ctx.PeekingLexer)
	if err != nil {
		if err == NextMatch {
			return nil, nil
		}
		return nil, err
	}
	return []reflect.Value{rv.Elem()}, nil
}

// @@
type strct struct {
	typ              reflect.Type
	expr             node
	tokensFieldIndex []int
	posFieldIndex    []int
	endPosFieldIndex []int
}

func newStrct(typ reflect.Type) *strct {
	s := &strct{
		typ: typ,
	}
	field, ok := typ.FieldByName("Pos")
	if ok && field.Type == positionType {
		s.posFieldIndex = field.Index
	}
	field, ok = typ.FieldByName("EndPos")
	if ok && field.Type == positionType {
		s.endPosFieldIndex = field.Index
	}
	field, ok = typ.FieldByName("Tokens")
	if ok && field.Type == tokensType {
		s.tokensFieldIndex = field.Index
	}
	return s
}

func (s *strct) String() string { return ebnf(s) }

func (s *strct) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	sv := reflect.New(s.typ).Elem()
	start := ctx.RawCursor()
	t, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	s.maybeInjectStartToken(t, sv)
	if out, err = s.expr.Parse(ctx, sv); err != nil {
		_ = ctx.Apply() // Best effort to give partial AST.
		ctx.MaybeUpdateError(err)
		return []reflect.Value{sv}, err
	} else if out == nil {
		return nil, nil
	}
	end := ctx.RawCursor()
	t, _ = ctx.RawPeek(0)
	s.maybeInjectEndToken(t, sv)
	s.maybeInjectTokens(ctx.Range(start, end), sv)
	return []reflect.Value{sv}, ctx.Apply()
}

func (s *strct) maybeInjectStartToken(token lexer.Token, v reflect.Value) {
	if s.posFieldIndex == nil {
		return
	}
	v.FieldByIndex(s.posFieldIndex).Set(reflect.ValueOf(token.Pos))
}

func (s *strct) maybeInjectEndToken(token lexer.Token, v reflect.Value) {
	if s.endPosFieldIndex == nil {
		return
	}
	v.FieldByIndex(s.endPosFieldIndex).Set(reflect.ValueOf(token.Pos))
}

func (s *strct) maybeInjectTokens(tokens []lexer.Token, v reflect.Value) {
	if s.tokensFieldIndex == nil {
		return
	}
	v.FieldByIndex(s.tokensFieldIndex).Set(reflect.ValueOf(tokens))
}

type groupMatchMode int

const (
	groupMatchOnce       groupMatchMode = iota
	groupMatchZeroOrOne                 = iota
	groupMatchZeroOrMore                = iota
	groupMatchOneOrMore                 = iota
	groupMatchNonEmpty                  = iota
)

// ( <expr> ) - match once
// ( <expr> )* - match zero or more times
// ( <expr> )+ - match one or more times
// ( <expr> )? - match zero or once
// ( <expr> )! - must be a non-empty match
//
// The additional modifier "!" forces the content of the group to be non-empty if it does match.
type group struct {
	expr node
	mode groupMatchMode
}

func (g *group) String() string { return ebnf(g) }
func (g *group) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	// Configure min/max matches.
	min := 1
	max := 1
	switch g.mode {
	case groupMatchNonEmpty:
		out, err = g.expr.Parse(ctx, parent)
		if err != nil {
			return out, err
		}
		if len(out) == 0 {
			t, _ := ctx.Peek(0)
			return out, Errorf(t.Pos, "sub-expression %s cannot be empty", g)
		}
		return out, nil
	case groupMatchOnce:
		return g.expr.Parse(ctx, parent)
	case groupMatchZeroOrOne:
		min = 0
	case groupMatchZeroOrMore:
		min = 0
		max = MaxIterations
	case groupMatchOneOrMore:
		min = 1
		max = MaxIterations
	}
	matches := 0
	for ; matches < max; matches++ {
		branch := ctx.Branch()
		v, err := g.expr.Parse(branch, parent)
		out = append(out, v...)
		if err != nil {
			ctx.MaybeUpdateError(err)
			// Optional part failed to match.
			if ctx.Stop(err, branch) {
				return out, err
			}
			break
		} else {
			ctx.Accept(branch)
		}
		if v == nil {
			break
		}
	}
	// fmt.Printf("%d < %d < %d: out == nil? %v\n", min, matches, max, out == nil)
	t, _ := ctx.Peek(0)
	if matches >= MaxIterations {
		panic(Errorf(t.Pos, "too many iterations of %s (> %d)", g, MaxIterations))
	}
	if matches < min {
		return out, Errorf(t.Pos, "sub-expression %s must match at least once", g)
	}
	// The idea here is that something like "a"? is a successful match and that parsing should proceed.
	if min == 0 && out == nil {
		out = []reflect.Value{}
	}
	return out, nil
}

// <expr> {"|" <expr>}
type disjunction struct {
	nodes []node
}

func (d *disjunction) String() string { return ebnf(d) }

func (d *disjunction) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	var (
		deepestError = 0
		firstError   error
		firstValues  []reflect.Value
	)
	for _, a := range d.nodes {
		branch := ctx.Branch()
		if value, err := a.Parse(branch, parent); err != nil {
			// If this branch progressed too far and still didn't match, error out.
			if ctx.Stop(err, branch) {
				return value, err
			}
			// Show the closest error returned. The idea here is that the further the parser progresses
			// without error, the more difficult it is to trace the error back to its root.
			if branch.Cursor() >= deepestError {
				firstError = err
				firstValues = value
				deepestError = branch.Cursor()
			}
		} else if value != nil {
			bt, _ := branch.Peek(0)
			ct, _ := ctx.Peek(0)
			if bt == ct {
				panic(Errorf(bt.Pos, "branch %s was accepted but did not progress the lexer at %s (%q)", a, bt.Pos, bt.Value))
			}
			ctx.Accept(branch)
			return value, nil
		}
	}
	if firstError != nil {
		ctx.MaybeUpdateError(firstError)
		return firstValues, firstError
	}
	return nil, nil
}

// <node> ...
type sequence struct {
	head bool
	node node
	next *sequence
}

func (s *sequence) String() string { return ebnf(s) }

func (s *sequence) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	for n := s; n != nil; n = n.next {
		child, err := n.node.Parse(ctx, parent)
		out = append(out, child...)
		if err != nil {
			return out, err
		}
		if child == nil {
			// Early exit if first value doesn't match, otherwise all values must match.
			if n == s {
				return nil, nil
			}
			token, err := ctx.Peek(0)
			if err != nil {
				return nil, err
			}
			return out, UnexpectedTokenError{Unexpected: token, Expected: n.String()}
		}
	}
	return out, nil
}

// @<expr>
type capture struct {
	field structLexerField
	node  node
}

func (c *capture) String() string { return ebnf(c) }

func (c *capture) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	start := ctx.RawCursor()
	v, err := c.node.Parse(ctx, parent)
	if v != nil {
		ctx.Defer(ctx.Range(start, ctx.RawCursor()), parent, c.field, v)
	}
	if err != nil {
		return []reflect.Value{parent}, err
	}
	if v == nil {
		return nil, nil
	}
	return []reflect.Value{parent}, nil
}

// <identifier> - named lexer token reference
type reference struct {
	typ        rune
	identifier string // Used for informational purposes.
}

func (r *reference) String() string { return ebnf(r) }

func (r *reference) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	token, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	if token.Type != r.typ {
		return nil, nil
	}
	_, _ = ctx.Next()
	return []reflect.Value{reflect.ValueOf(token.Value)}, nil
}

// [ <expr> ] <sequence>
type optional struct {
	node node
}

func (o *optional) String() string { return ebnf(o) }

func (o *optional) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	branch := ctx.Branch()
	out, err = o.node.Parse(branch, parent)
	if err != nil {
		// Optional part failed to match.
		if ctx.Stop(err, branch) {
			return out, err
		}
	} else {
		ctx.Accept(branch)
	}
	if out == nil {
		out = []reflect.Value{}
	}
	return out, nil
}

// { <expr> } <sequence>
type repetition struct {
	node node
}

func (r *repetition) String() string { return ebnf(r) }

// Parse a repetition. Once a repetition is encountered it will always match, so grammars
// should ensure that branches are differentiated prior to the repetition.
func (r *repetition) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	i := 0
	for ; i < MaxIterations; i++ {
		branch := ctx.Branch()
		v, err := r.node.Parse(branch, parent)
		out = append(out, v...)
		if err != nil {
			// Optional part failed to match.
			if ctx.Stop(err, branch) {
				return out, err
			}
			break
		} else {
			ctx.Accept(branch)
		}
		if v == nil {
			break
		}
	}
	if i >= MaxIterations {
		t, _ := ctx.Peek(0)
		panic(Errorf(t.Pos, "too many iterations of %s (> %d)", r, MaxIterations))
	}
	if out == nil {
		out = []reflect.Value{}
	}
	return out, nil
}

// Match a token literal exactly "..."[:<type>].
type literal struct {
	s  string
	t  rune
	tt string // Used for display purposes - symbolic name of t.
}

func (l *literal) String() string { return ebnf(l) }

func (l *literal) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	token, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	equal := false // nolint: ineffassign
	if ctx.caseInsensitive[token.Type] {
		equal = strings.EqualFold(token.Value, l.s)
	} else {
		equal = token.Value == l.s
	}
	if equal && (l.t == -1 || l.t == token.Type) {
		next, err := ctx.Next()
		if err != nil {
			return nil, err
		}
		return []reflect.Value{reflect.ValueOf(next.Value)}, nil
	}
	return nil, nil
}

type negation struct {
	node node
}

func (n *negation) String() string { return ebnf(n) }

func (n *negation) Parse(ctx *parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	// Create a branch to avoid advancing the parser, but call neither Stop nor Accept on it
	// since we will discard a match.
	branch := ctx.Branch()
	notEOF, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	if notEOF.EOF() {
		// EOF cannot match a negation, which expects something
		return nil, nil
	}

	out, err = n.node.Parse(branch, parent)

	if out != nil && err == nil {
		// out being non-nil means that what we don't want is actually here, so we report nomatch
		return nil, Errorf(notEOF.Pos, "unexpected '%s'", notEOF.Value)
	}

	// Just give the next token
	next, err := ctx.Next()
	if err != nil {
		return nil, err
	}
	return []reflect.Value{reflect.ValueOf(next.Value)}, nil
}

// Attempt to transform values to given type.
//
// This will dereference pointers, and attempt to parse strings into integer values, floats, etc.
func conform(t reflect.Type, values []reflect.Value) (out []reflect.Value, err error) {
	for _, v := range values {
		for t != v.Type() && t.Kind() == reflect.Ptr && v.Kind() != reflect.Ptr {
			// This can occur during partial failure.
			if !v.CanAddr() {
				return
			}
			v = v.Addr()
		}

		// Already of the right kind, don't bother converting.
		if v.Kind() == t.Kind() {
			if v.Type() != t {
				v = v.Convert(t)
			}
			out = append(out, v)
			continue
		}

		kind := t.Kind()
		switch kind { // nolint: exhaustive
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, err := strconv.ParseInt(v.String(), 0, sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetInt(n)

		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseUint(v.String(), 0, sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetUint(n)

		case reflect.Bool:
			v = reflect.ValueOf(true)

		case reflect.Float32, reflect.Float64:
			n, err := strconv.ParseFloat(v.String(), sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetFloat(n)
		}

		out = append(out, v)
	}
	return out, nil
}

func sizeOfKind(kind reflect.Kind) int {
	switch kind { // nolint: exhaustive
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32, reflect.Float32:
		return 32
	case reflect.Int64, reflect.Uint64, reflect.Float64:
		return 64
	case reflect.Int, reflect.Uint:
		return strconv.IntSize
	}
	panic("unsupported kind " + kind.String())
}

// Set field.
//
// If field is a pointer the pointer will be set to the value. If field is a string, value will be
// appended. If field is a slice, value will be appended to slice.
//
// For all other types, an attempt will be made to convert the string to the corresponding
// type (int, float32, etc.).
func setField(tokens []lexer.Token, strct reflect.Value, field structLexerField, fieldValue []reflect.Value) (err error) { // nolint: gocognit
	defer decorate(&err, nearestPos(tokens), func() string { return strct.Type().Name() + "." + field.Name })

	f := strct.FieldByIndex(field.Index)

	// Any kind of pointer, hydrate it first.
	if f.Kind() == reflect.Ptr {
		if f.IsNil() {
			fv := reflect.New(f.Type().Elem()).Elem()
			f.Set(fv.Addr())
			f = fv
		} else {
			f = f.Elem()
		}
	}

	if f.Type() == tokenType {
		f.Set(reflect.ValueOf(tokens[0]))
		return nil
	}

	if f.Type() == tokensType {
		f.Set(reflect.ValueOf(tokens))
		return nil
	}

	if f.CanAddr() {
		if d, ok := f.Addr().Interface().(Capture); ok {
			ifv := make([]string, 0, len(fieldValue))
			for _, v := range fieldValue {
				ifv = append(ifv, v.Interface().(string))
			}
			return d.Capture(ifv)
		} else if d, ok := f.Addr().Interface().(encoding.TextUnmarshaler); ok {
			for _, v := range fieldValue {
				if err := d.UnmarshalText([]byte(v.Interface().(string))); err != nil {
					return err
				}
			}
			return nil
		}
	}

	if f.Kind() == reflect.Slice {
		sliceElemType := f.Type().Elem()
		if sliceElemType.Implements(captureType) || reflect.PtrTo(sliceElemType).Implements(captureType) {
			if sliceElemType.Kind() == reflect.Ptr {
				sliceElemType = sliceElemType.Elem()
			}
			for _, v := range fieldValue {
				d := reflect.New(sliceElemType).Interface().(Capture)
				if err := d.Capture([]string{v.Interface().(string)}); err != nil {
					return err
				}
				eltValue := reflect.ValueOf(d)
				if f.Type().Elem().Kind() != reflect.Ptr {
					eltValue = eltValue.Elem()
				}
				f.Set(reflect.Append(f, eltValue))
			}
		} else {
			fieldValue, err = conform(sliceElemType, fieldValue)
			if err != nil {
				return err
			}
			f.Set(reflect.Append(f, fieldValue...))
		}
		return nil
	}

	// Strings concatenate all captured tokens.
	if f.Kind() == reflect.String {
		fieldValue, err = conform(f.Type(), fieldValue)
		if err != nil {
			return err
		}
		for _, v := range fieldValue {
			f.Set(reflect.ValueOf(f.String() + v.String()).Convert(f.Type()))
		}
		return nil
	}

	// Coalesce multiple tokens into one. This allows eg. ["-", "10"] to be captured as separate tokens but
	// parsed as a single string "-10".
	if len(fieldValue) > 1 {
		out := []string{}
		for _, v := range fieldValue {
			out = append(out, v.String())
		}
		fieldValue = []reflect.Value{reflect.ValueOf(strings.Join(out, ""))}
	}

	fieldValue, err = conform(f.Type(), fieldValue)
	if err != nil {
		return err
	}

	fv := fieldValue[0]

	switch f.Kind() { // nolint: exhaustive
	// Numeric types will increment if the token can not be coerced.
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Type() != f.Type() {
			f.SetInt(f.Int() + 1)
		} else {
			f.Set(fv)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if fv.Type() != f.Type() {
			f.SetUint(f.Uint() + 1)
		} else {
			f.Set(fv)
		}

	case reflect.Float32, reflect.Float64:
		if fv.Type() != f.Type() {
			f.SetFloat(f.Float() + 1)
		} else {
			f.Set(fv)
		}

	case reflect.Bool, reflect.Struct:
		if f.Kind() == reflect.Bool && fv.Kind() == reflect.Bool {
			f.SetBool(fv.Bool())
			break
		}
		if fv.Type() != f.Type() {
			return fmt.Errorf("value %q is not correct type %s", fv, f.Type())
		}
		f.Set(fv)

	default:
		return fmt.Errorf("unsupported field type %s for field %s", f.Type(), field.Name)
	}
	return nil
}
