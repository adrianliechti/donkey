// Package pim reads what address books and calendars hold: vCard (RFC 6350
// and its 3.0 predecessor) and iCalendar (RFC 5545). Both are the same text
// underneath — folded content lines, NAME;PARAM=value:VALUE, nested in
// BEGIN/END blocks — so one parser reads both, leniently: a line it cannot
// read is skipped, not the whole card.
package pim

import (
	"bytes"
	"strings"
)

// Property is one content line.
type Property struct {
	Group  string              // "item1" of "item1.EMAIL"
	Name   string              // upper case
	Params map[string][]string // names upper case, values as written
	Value  string              // still escaped
}

// Component is a BEGIN/END block: a VCARD, a VCALENDAR, a VEVENT inside it.
type Component struct {
	Name     string // upper case
	Props    []Property
	Children []*Component
}

// Parse reads the top-level components of a vCard or iCalendar text.
func Parse(data []byte) []*Component {
	var roots, stack []*Component
	for _, line := range unfold(data) {
		p, ok := parseLine(line)
		if !ok {
			continue
		}
		switch p.Name {
		case "BEGIN":
			c := &Component{Name: strings.ToUpper(strings.TrimSpace(p.Value))}
			if len(stack) == 0 {
				roots = append(roots, c)
			} else {
				top := stack[len(stack)-1]
				top.Children = append(top.Children, c)
			}
			stack = append(stack, c)
		case "END":
			// An END closes the innermost block of its name; one that
			// matches nothing open is noise.
			name := strings.ToUpper(strings.TrimSpace(p.Value))
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].Name == name {
					stack = stack[:i]
					break
				}
			}
		default:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.Props = append(top.Props, p)
			}
		}
	}
	return roots
}

// unfold joins folded lines: a line break followed by a space or a tab
// continues the line before it.
func unfold(data []byte) []string {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	var lines []string
	var cur strings.Builder
	for i, raw := range bytes.Split(data, []byte("\n")) {
		if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') && i > 0 {
			cur.Write(raw[1:])
			continue
		}
		if cur.Len() > 0 {
			lines = append(lines, cur.String())
		}
		cur.Reset()
		cur.Write(raw)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// parseLine splits NAME;PARAM=a,"b;c":VALUE. Quotes protect the
// separators inside a parameter value; the value is everything after the
// first colon outside them.
func parseLine(line string) (Property, bool) {
	inQuote := false
	var segments []string
	start := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				segments = append(segments, line[start:i])
				start = i + 1
			}
		case ':':
			if inQuote {
				continue
			}
			segments = append(segments, line[start:i])
			return property(segments, line[i+1:])
		}
	}
	return Property{}, false
}

func property(segments []string, value string) (Property, bool) {
	name := strings.TrimSpace(segments[0])
	if name == "" {
		return Property{}, false
	}
	p := Property{Name: strings.ToUpper(name), Value: value}
	if group, rest, ok := strings.Cut(p.Name, "."); ok {
		p.Group, p.Name = group, rest
	}
	for _, seg := range segments[1:] {
		key, val, ok := strings.Cut(seg, "=")
		if !ok {
			// vCard 2.1 writes types bare: TEL;WORK;VOICE.
			key, val = "TYPE", seg
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		if p.Params == nil {
			p.Params = map[string][]string{}
		}
		for _, v := range splitUnquoted(val, ',') {
			p.Params[key] = append(p.Params[key], decodeParam(strings.Trim(strings.TrimSpace(v), `"`)))
		}
	}
	return p, true
}

// splitUnquoted splits at sep outside double quotes.
func splitUnquoted(s string, sep byte) []string {
	var out []string
	inQuote, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// decodeParam undoes RFC 6868's caret escapes: ^n, ^^ and ^'.
func decodeParam(s string) string {
	if !strings.Contains(s, "^") {
		return s
	}
	return strings.NewReplacer("^n", "\n", "^N", "\n", "^^", "^", "^'", `"`).Replace(s)
}

// Param is a parameter's first value.
func (p Property) Param(name string) string {
	if v := p.Params[strings.ToUpper(name)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Text is the value unescaped: \n, \, \; and \\ mean what they say.
func (p Property) Text() string { return unescape(p.Value) }

// Parts splits a structured value (N, ADR, ORG) at its semicolons, each
// part unescaped.
func (p Property) Parts() []string {
	var out []string
	for _, part := range splitEscaped(p.Value, ';') {
		out = append(out, unescape(part))
	}
	return out
}

// List splits a list value (CATEGORIES, NICKNAME, EXDATE) at its commas.
func (p Property) List() []string {
	var out []string
	for _, part := range splitEscaped(p.Value, ',') {
		if v := strings.TrimSpace(unescape(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitEscaped(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case sep:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Get is the component's first property of that name.
func (c *Component) Get(name string) (Property, bool) {
	for _, p := range c.Props {
		if p.Name == name {
			return p, true
		}
	}
	return Property{}, false
}

// Text is the unescaped value of the first property of that name.
func (c *Component) Text(name string) string {
	if p, ok := c.Get(name); ok {
		return strings.TrimSpace(p.Text())
	}
	return ""
}

// All is every property of that name, in order.
func (c *Component) All(name string) []Property {
	var out []Property
	for _, p := range c.Props {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// Child is the first nested component of that name.
func (c *Component) Child(name string) *Component {
	for _, k := range c.Children {
		if k.Name == name {
			return k
		}
	}
	return nil
}
