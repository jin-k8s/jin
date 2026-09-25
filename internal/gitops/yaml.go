package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DocMatch selects a document in a multi-document YAML file.
type DocMatch struct {
	APIVersionPrefix string `json:"apiVersionPrefix,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Name             string `json:"name,omitempty"`
}

func (m DocMatch) empty() bool { return m == DocMatch{} }

// editYAML replaces the scalar at path in place, so comments, ordering and indentation are untouched.
func editYAML(src []byte, match DocMatch, keyPath, value string) ([]byte, string, error) {
	node, err := findYAMLScalar(src, match, keyPath)
	if err != nil {
		return nil, "", err
	}
	if value == "" || node.Value == value {
		return src, node.Value, nil
	}
	start := offsetOf(src, node.Line, node.Column)
	if start < 0 {
		return nil, "", fmt.Errorf("cannot locate %s in the source", keyPath)
	}
	end, err := scalarEnd(src, start, node)
	if err != nil {
		return nil, "", err
	}
	var repl string
	switch {
	case node.Style&yaml.DoubleQuotedStyle != 0:
		repl = strconv.Quote(value)
	case node.Style&yaml.SingleQuotedStyle != 0:
		repl = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	default:
		repl = value
	}
	out := make([]byte, 0, len(src)+len(repl))
	out = append(out, src[:start]...)
	out = append(out, repl...)
	out = append(out, src[end:]...)
	return out, node.Value, nil
}

func findYAMLScalar(src []byte, match DocMatch, keyPath string) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var lastErr error
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if !match.empty() && !docMatches(root, match) {
			continue
		}
		n, err := walk(root, keyPath)
		if err != nil {
			lastErr = err
			continue
		}
		if n.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s is not a scalar", keyPath)
		}
		return n, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no YAML document matches %+v", match)
}

func docMatches(root *yaml.Node, m DocMatch) bool {
	get := func(p string) string {
		n, err := walk(root, p)
		if err != nil || n.Kind != yaml.ScalarNode {
			return ""
		}
		return n.Value
	}
	return (m.APIVersionPrefix == "" || strings.HasPrefix(get("apiVersion"), m.APIVersionPrefix)) &&
		(m.Kind == "" || get("kind") == m.Kind) &&
		(m.Name == "" || get("metadata.name") == m.Name)
}

// walk follows a dotted path; segments may carry [N] indexes or [key=value] selectors.
func walk(n *yaml.Node, keyPath string) (*yaml.Node, error) {
	cur := n
	for _, seg := range splitPath(keyPath) {
		key, sel, hasSel := strings.Cut(seg, "[")
		if key != "" {
			if cur.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%s: %q is not a mapping", keyPath, key)
			}
			var next *yaml.Node
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Value == key {
					next = cur.Content[i+1]
					break
				}
			}
			if next == nil {
				return nil, fmt.Errorf("%s: key %q not found", keyPath, key)
			}
			cur = next
		}
		for hasSel {
			var rest string
			sel, rest, _ = strings.Cut(sel, "]")
			if cur.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("%s: [%s] applied to a non-list", keyPath, sel)
			}
			if k, v, ok := strings.Cut(sel, "="); ok {
				var found *yaml.Node
				for _, item := range cur.Content {
					if c, err := walk(item, k); err == nil && c.Value == v {
						found = item
						break
					}
				}
				if found == nil {
					return nil, fmt.Errorf("%s: no list item with %s", keyPath, sel)
				}
				cur = found
			} else {
				i, err := strconv.Atoi(sel)
				if err != nil || i < 0 || i >= len(cur.Content) {
					return nil, fmt.Errorf("%s: bad index [%s]", keyPath, sel)
				}
				cur = cur.Content[i]
			}
			sel, hasSel = strings.CutPrefix(rest, "[")
		}
	}
	return cur, nil
}

// splitPath splits on dots outside [] selectors.
func splitPath(p string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range p {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				out = append(out, p[start:i])
				start = i + 1
			}
		}
	}
	return append(out, p[start:])
}

func offsetOf(src []byte, line, col int) int {
	off := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(src[off:], '\n')
		if i < 0 {
			return -1
		}
		off += i + 1
	}
	off += col - 1
	if off > len(src) {
		return -1
	}
	return off
}

func scalarEnd(src []byte, start int, n *yaml.Node) (int, error) {
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		for i := start + 1; i < len(src); i++ {
			if src[i] == '\\' {
				i++
				continue
			}
			if src[i] == '"' {
				return i + 1, nil
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		for i := start + 1; i < len(src); i++ {
			if src[i] == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			}
		}
	case n.Style == 0 || n.Style&yaml.TaggedStyle != 0:
		end := start + len(n.Value)
		if end <= len(src) && string(src[start:end]) == n.Value {
			return end, nil
		}
	}
	return 0, fmt.Errorf("cannot edit the %q scalar in place (block or unusual style)", n.Value)
}
