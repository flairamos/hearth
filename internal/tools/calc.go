package tools

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// calculatorTool evaluates arithmetic expressions safely, in-process
// (no shell, works identically on Windows/Linux).
func calculatorTool() Tool {
	return Tool{
		Name:     "calculator",
		Category: CatRead,
		Desc:     "Evaluate an arithmetic expression. Supports + - * / % ^ parentheses, functions (sqrt, abs, floor, ceil, round, sin, cos, tan, log, log10, exp, min, max, pow) and constants (pi, e). Use for any non-trivial math instead of mental arithmetic.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expression": map[string]any{"type": "string", "description": "Math expression, e.g. \"(1+2)*3^2 / 4\" or \"sqrt(144) + log(100)\""},
			},
			"required": []string{"expression"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			expr := strings.TrimSpace(argStr(args, "expression"))
			if expr == "" {
				return "", fmt.Errorf("expression is required")
			}
			v, err := evalExpr(expr)
			if err != nil {
				return "", fmt.Errorf("cannot evaluate %q: %v", expr, err)
			}
			return formatNumber(v), nil
		},
	}
}

func formatNumber(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return fmt.Sprintf("%v", v)
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 10, 64)
}

// ---- expression parser (recursive descent) ----

type parser struct {
	input string
	pos   int
}

func evalExpr(s string) (float64, error) {
	p := &parser{input: strings.ReplaceAll(s, " ", "")}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("unexpected %q at %d", p.input[p.pos], p.pos)
	}
	return v, nil
}

func (p *parser) peek() byte {
	if p.pos < len(p.input) {
		return p.input[p.pos]
	}
	return 0
}

func (p *parser) expr() (float64, error) { // + -
	left, err := p.term()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.term()
			if err != nil {
				return 0, err
			}
			left += r
		case '-':
			p.pos++
			r, err := p.term()
			if err != nil {
				return 0, err
			}
			left -= r
		default:
			return left, nil
		}
	}
}

func (p *parser) term() (float64, error) { // * / %
	left, err := p.unary()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '*':
			p.pos++
			// support ** as power
			if p.peek() == '*' {
				p.pos++
				r, err := p.unary()
				if err != nil {
					return 0, err
				}
				left = math.Pow(left, r)
				continue
			}
			r, err := p.unary()
			if err != nil {
				return 0, err
			}
			left *= r
		case '/':
			p.pos++
			r, err := p.unary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= r
		case '%':
			p.pos++
			r, err := p.unary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("modulo by zero")
			}
			left = math.Mod(left, r)
		default:
			return left, nil
		}
	}
}

func (p *parser) unary() (float64, error) {
	if p.peek() == '-' {
		p.pos++
		v, err := p.unary()
		return -v, err
	}
	if p.peek() == '+' {
		p.pos++
		return p.unary()
	}
	return p.power()
}

func (p *parser) power() (float64, error) { // ^ right associative
	base, err := p.atom()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
		exp, err := p.unary() // right-assoc via unary → power chain
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *parser) atom() (float64, error) {
	switch c := p.peek(); {
	case c == '(':
		p.pos++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("missing )")
		}
		p.pos++
		return v, nil
	case c == 0:
		return 0, fmt.Errorf("unexpected end of expression")
	case unicode.IsDigit(rune(c)) || c == '.':
		return p.number()
	case unicode.IsLetter(rune(c)):
		return p.ident()
	default:
		return 0, fmt.Errorf("unexpected char %q", string(c))
	}
}

func (p *parser) number() (float64, error) {
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsDigit(rune(p.input[p.pos])) || p.input[p.pos] == '.') {
		p.pos++
	}
	v, err := strconv.ParseFloat(p.input[start:p.pos], 64)
	if err != nil {
		return 0, fmt.Errorf("bad number %q", p.input[start:p.pos])
	}
	return v, nil
}

func (p *parser) ident() (float64, error) {
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsLetter(rune(p.input[p.pos])) || unicode.IsDigit(rune(p.input[p.pos]))) {
		p.pos++
	}
	name := strings.ToLower(p.input[start:p.pos])

	switch name {
	case "pi":
		return math.Pi, nil
	case "e":
		return math.E, nil
	}

	// function call
	if p.peek() != '(' {
		return 0, fmt.Errorf("unknown identifier %q", name)
	}
	p.pos++
	var vals []float64
	if p.peek() != ')' {
		for {
			v, err := p.expr()
			if err != nil {
				return 0, err
			}
			vals = append(vals, v)
			if p.peek() == ',' {
				p.pos++
				continue
			}
			break
		}
	}
	if p.peek() != ')' {
		return 0, fmt.Errorf("missing ) after %s(", name)
	}
	p.pos++
	return applyFunc(name, vals)
}

func applyFunc(name string, v []float64) (float64, error) {
	need := func(n int) error {
		if len(v) != n {
			return fmt.Errorf("%s expects %d arg(s), got %d", name, n, len(v))
		}
		return nil
	}
	switch name {
	case "sqrt":
		if err := need(1); err != nil {
			return 0, err
		}
		if v[0] < 0 {
			return 0, fmt.Errorf("sqrt of negative")
		}
		return math.Sqrt(v[0]), nil
	case "abs":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Abs(v[0]), nil
	case "floor":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Floor(v[0]), nil
	case "ceil":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Ceil(v[0]), nil
	case "round":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Round(v[0]), nil
	case "sin":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Sin(v[0]), nil
	case "cos":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Cos(v[0]), nil
	case "tan":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Tan(v[0]), nil
	case "log":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Log(v[0]), nil
	case "log10":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Log10(v[0]), nil
	case "exp":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Exp(v[0]), nil
	case "min":
		if len(v) < 1 {
			return 0, fmt.Errorf("min needs args")
		}
		m := v[0]
		for _, x := range v[1:] {
			if x < m {
				m = x
			}
		}
		return m, nil
	case "max":
		if len(v) < 1 {
			return 0, fmt.Errorf("max needs args")
		}
		m := v[0]
		for _, x := range v[1:] {
			if x > m {
				m = x
			}
		}
		return m, nil
	case "pow":
		if err := need(2); err != nil {
			return 0, err
		}
		return math.Pow(v[0], v[1]), nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}
