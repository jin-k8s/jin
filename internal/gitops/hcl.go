package gitops

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// reference is an attribute that points at a variable or local instead of holding a literal.
type reference struct {
	kind string // "var" or "local"
	name string
}

var errNotLiteral = errors.New("not a string literal")

func parseHCL(src []byte, filename string) (*hclwrite.File, error) {
	f, diags := hclwrite.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse %s: %s", filename, diags.Error())
	}
	return f, nil
}

// findBlock resolves addresses such as module.eks, aws_eks_cluster.this or data.x.y.
func findBlock(body *hclwrite.Body, address string) *hclwrite.Block {
	parts := strings.Split(address, ".")
	var typ string
	var labels []string
	switch {
	case len(parts) == 2 && parts[0] == "module":
		typ, labels = "module", parts[1:]
	case len(parts) == 3 && parts[0] == "data":
		typ, labels = "data", parts[1:]
	case len(parts) == 2:
		typ, labels = "resource", parts
	default:
		return nil
	}
	for _, b := range body.Blocks() {
		if b.Type() != typ || len(b.Labels()) != len(labels) {
			continue
		}
		match := true
		for i, l := range b.Labels() {
			if l != labels[i] {
				match = false
			}
		}
		if match {
			return b
		}
	}
	return nil
}

func significant(toks hclwrite.Tokens) hclwrite.Tokens {
	var out hclwrite.Tokens
	for _, t := range toks {
		if t.Type == hclsyntax.TokenNewline || t.Type == hclsyntax.TokenComment {
			continue
		}
		out = append(out, t)
	}
	return out
}

func literalString(toks hclwrite.Tokens) (string, bool) {
	t := significant(toks)
	switch {
	case len(t) == 3 && t[0].Type == hclsyntax.TokenOQuote && t[1].Type == hclsyntax.TokenQuotedLit && t[2].Type == hclsyntax.TokenCQuote:
		return string(t[1].Bytes), true
	case len(t) == 2 && t[0].Type == hclsyntax.TokenOQuote && t[1].Type == hclsyntax.TokenCQuote:
		return "", true
	}
	return "", false
}

func referenceOf(toks hclwrite.Tokens) (reference, bool) {
	t := significant(toks)
	if len(t) == 3 && t[0].Type == hclsyntax.TokenIdent && t[1].Type == hclsyntax.TokenDot && t[2].Type == hclsyntax.TokenIdent {
		switch string(t[0].Bytes) {
		case "var", "local":
			return reference{kind: string(t[0].Bytes), name: string(t[2].Bytes)}, true
		}
	}
	return reference{}, false
}

// hclAttr reads or writes a string attribute. When value is empty the file is only read.
// It returns the current literal, or the reference the attribute points at.
func hclAttr(body *hclwrite.Body, attr, value string) (current string, ref *reference, changed bool, err error) {
	a := body.GetAttribute(attr)
	if a == nil {
		return "", nil, false, fmt.Errorf("attribute %q is not set", attr)
	}
	toks := a.Expr().BuildTokens(nil)
	if lit, ok := literalString(toks); ok {
		if value != "" && lit != value {
			body.SetAttributeValue(attr, cty.StringVal(value))
			return lit, nil, true, nil
		}
		return lit, nil, false, nil
	}
	if r, ok := referenceOf(toks); ok {
		return "", &r, false, nil
	}
	return "", nil, false, fmt.Errorf("attribute %q is an expression (%s): %w", attr, strings.TrimSpace(string(toks.Bytes())), errNotLiteral)
}

// editTerraform sets address.attribute in file to value, following var.* and local.* references
// within the file's module directory. It returns the edited files and the previous value.
func editTerraform(ws Workspace, file, address, attribute, value string) (map[string][]byte, string, error) {
	src, err := ws.Read(file)
	if err != nil {
		return nil, "", err
	}
	f, err := parseHCL(src, file)
	if err != nil {
		return nil, "", err
	}
	b := findBlock(f.Body(), address)
	if b == nil {
		return nil, "", fmt.Errorf("%s: block %s not found", file, address)
	}
	cur, ref, changed, err := hclAttr(b.Body(), attribute, value)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %s: %w", file, address, err)
	}
	if ref == nil {
		edits := map[string][]byte{}
		if changed {
			edits[file] = f.Bytes()
		}
		return edits, cur, nil
	}
	return editReference(ws, path.Dir(file), *ref, value)
}

// editReference updates the literal a variable or local resolves to. Variables are resolved the
// way Terraform does for a root module without -var-file: *.auto.tfvars and terraform.tfvars
// (later files win), then the variable default.
func editReference(ws Workspace, dir string, ref reference, value string) (map[string][]byte, string, error) {
	files, err := ws.List(dir)
	if err != nil {
		return nil, "", err
	}
	sort.Strings(files)
	edit := func(file string, body func(*hclwrite.File) *hclwrite.Body, attr string) (map[string][]byte, string, bool, error) {
		src, err := ws.Read(file)
		if err != nil {
			return nil, "", false, err
		}
		f, err := parseHCL(src, file)
		if err != nil {
			return nil, "", false, err
		}
		b := body(f)
		if b == nil || b.GetAttribute(attr) == nil {
			return nil, "", false, nil
		}
		cur, r, changed, err := hclAttr(b, attr, value)
		if err != nil {
			return nil, "", false, fmt.Errorf("%s: %w", file, err)
		}
		if r != nil {
			return nil, "", false, fmt.Errorf("%s: %s.%s points at another reference (%s.%s); edit it manually", file, ref.kind, ref.name, r.kind, r.name)
		}
		edits := map[string][]byte{}
		if changed {
			edits[file] = f.Bytes()
		}
		return edits, cur, true, nil
	}

	if ref.kind == "var" {
		var tfvars []string
		for _, f := range files {
			if strings.HasSuffix(f, ".auto.tfvars") {
				tfvars = append(tfvars, f)
			}
		}
		for _, f := range files {
			if path.Base(f) == "terraform.tfvars" {
				tfvars = append([]string{f}, tfvars...)
			}
		}
		// Highest precedence last: auto.tfvars (lexical) override terraform.tfvars.
		for i := len(tfvars) - 1; i >= 0; i-- {
			edits, cur, found, err := edit(tfvars[i], func(f *hclwrite.File) *hclwrite.Body { return f.Body() }, ref.name)
			if err != nil || found {
				return edits, cur, err
			}
		}
	}
	for _, file := range files {
		if !strings.HasSuffix(file, ".tf") {
			continue
		}
		var bodyFn func(*hclwrite.File) *hclwrite.Body
		attr := ref.name
		if ref.kind == "var" {
			attr = "default"
			bodyFn = func(f *hclwrite.File) *hclwrite.Body {
				for _, b := range f.Body().Blocks() {
					if b.Type() == "variable" && len(b.Labels()) == 1 && b.Labels()[0] == ref.name {
						return b.Body()
					}
				}
				return nil
			}
		} else {
			bodyFn = func(f *hclwrite.File) *hclwrite.Body {
				for _, b := range f.Body().Blocks() {
					if b.Type() == "locals" && b.Body().GetAttribute(ref.name) != nil {
						return b.Body()
					}
				}
				return nil
			}
		}
		edits, cur, found, err := edit(file, bodyFn, attr)
		if err != nil || found {
			return edits, cur, err
		}
	}
	return nil, "", fmt.Errorf("%s.%s: no literal value found in %s (values passed with -var-file or the environment cannot be edited automatically)", ref.kind, ref.name, dir)
}
