// Package gitops upgrades clusters by editing their declarative sources and raising pull requests.
package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"gopkg.in/yaml.v3"
)

const (
	RoleControlPlane = "control-plane"
	RoleNodeGroup    = "node-group"
	RoleAddon        = "addon"

	KindTerraform = "terraform"
	KindYAML      = "yaml"
)

// Target is one version field in a repository that Jin keeps in sync with the upgrade.
type Target struct {
	Role string `json:"role"`
	// Name identifies node groups and add-ons (for add-ons it is the provider's add-on name).
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	File string `json:"file"`
	// Terraform: block address (module.eks, aws_eks_cluster.this) and attribute.
	Address   string `json:"address,omitempty"`
	Attribute string `json:"attribute,omitempty"`
	// YAML: dotted path with optional [N] / [key=value] selectors, and a document selector.
	Path  string   `json:"path,omitempty"`
	Match DocMatch `json:"match,omitempty"`
	// Format renders the value; "{version}" is replaced. Defaults to "{version}".
	Format string `json:"format,omitempty"`
}

func (t Target) String() string {
	if t.Kind == KindTerraform {
		return fmt.Sprintf("%s: %s.%s", t.File, t.Address, t.Attribute)
	}
	return fmt.Sprintf("%s: %s", t.File, t.Path)
}

func (t Target) Validate() error {
	switch {
	case t.Role != RoleControlPlane && t.Role != RoleNodeGroup && t.Role != RoleAddon:
		return fmt.Errorf("target %s: unknown role %q", t, t.Role)
	case t.Role == RoleAddon && t.Name == "":
		return fmt.Errorf("target %s: add-on targets need the add-on name", t)
	case t.File == "" || strings.Contains(t.File, ".."):
		return fmt.Errorf("target: invalid file %q", t.File)
	case t.Kind == KindTerraform && (t.Address == "" || t.Attribute == ""):
		return fmt.Errorf("target %s: terraform targets need address and attribute", t)
	case t.Kind == KindYAML && t.Path == "":
		return fmt.Errorf("target %s: yaml targets need a path", t)
	case t.Kind != KindTerraform && t.Kind != KindYAML:
		return fmt.Errorf("target %s: unknown kind %q", t, t.Kind)
	}
	return nil
}

func (t Target) render(version string) string {
	f := t.Format
	if f == "" {
		f = "{version}"
	}
	return strings.ReplaceAll(f, "{version}", version)
}

// Workspace is read access to a repository at one revision.
type Workspace interface {
	Read(file string) ([]byte, error)
	// List returns the files directly inside dir.
	List(dir string) ([]string, error)
}

// Change is the edit a target needs.
type Change struct {
	Target Target            `json:"target"`
	From   string            `json:"from"`
	To     string            `json:"to"`
	Files  map[string][]byte `json:"-"`
}

// Apply computes the edits that set target to version. A nil Change means nothing to change.
func Apply(ws Workspace, t Target, version string) (*Change, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	value := t.render(version)
	var (
		files map[string][]byte
		old   string
		err   error
	)
	switch t.Kind {
	case KindTerraform:
		files, old, err = editTerraform(ws, t.File, t.Address, t.Attribute, value)
	case KindYAML:
		var src, out []byte
		if src, err = ws.Read(t.File); err == nil {
			out, old, err = editYAML(src, t.Match, t.Path, value)
			if err == nil && !bytes.Equal(src, out) {
				files = map[string][]byte{t.File: out}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t, err)
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &Change{Target: t, From: old, To: value, Files: files}, nil
}

// Current reads the value a target holds today.
func Current(ws Workspace, t Target) (string, error) {
	switch t.Kind {
	case KindTerraform:
		_, v, err := editTerraform(ws, t.File, t.Address, t.Attribute, "")
		return v, err
	case KindYAML:
		src, err := ws.Read(t.File)
		if err != nil {
			return "", err
		}
		_, v, err := editYAML(src, t.Match, t.Path, "")
		return v, err
	}
	return "", fmt.Errorf("unknown kind %q", t.Kind)
}

// Candidate is a version field found by Discover.
type Candidate struct {
	Target   Target `json:"target"`
	Current  string `json:"current"`
	Provider string `json:"provider"`
	// Cluster is the cluster or node-group name found next to the field, used to match contexts.
	Cluster string `json:"cluster,omitempty"`
	Error   string `json:"error,omitempty"`
}

type tfPattern struct {
	role, attr, provider, nameAttr, addonAttr string
}

var tfResources = map[string]tfPattern{
	"aws_eks_cluster":                      {RoleControlPlane, "version", "eks", "name", ""},
	"aws_eks_node_group":                   {RoleNodeGroup, "version", "eks", "cluster_name", ""},
	"aws_eks_addon":                        {RoleAddon, "addon_version", "eks", "cluster_name", "addon_name"},
	"google_container_cluster":             {RoleControlPlane, "min_master_version", "gke", "name", ""},
	"google_container_node_pool":           {RoleNodeGroup, "version", "gke", "cluster", ""},
	"azurerm_kubernetes_cluster":           {RoleControlPlane, "kubernetes_version", "aks", "name", ""},
	"azurerm_kubernetes_cluster_node_pool": {RoleNodeGroup, "orchestrator_version", "aks", "name", ""},
}

// tfModules maps registry module sources to the attributes that carry versions.
var tfModules = []struct {
	source, provider, role string
	nameAttrs, attrs       []string
}{
	{"terraform-aws-modules/eks/aws//modules/eks-managed-node-group", "eks", RoleNodeGroup, []string{"cluster_name"}, []string{"kubernetes_version", "cluster_version"}},
	// v21 renamed cluster_name/cluster_version to name/kubernetes_version.
	{"terraform-aws-modules/eks/aws", "eks", RoleControlPlane, []string{"name", "cluster_name"}, []string{"kubernetes_version", "cluster_version"}},
	{"terraform-google-modules/kubernetes-engine/google", "gke", RoleControlPlane, []string{"name"}, []string{"kubernetes_version"}},
	{"Azure/aks/azurerm", "aks", RoleControlPlane, []string{"cluster_name"}, []string{"kubernetes_version"}},
}

type yamlPattern struct {
	apiPrefix, kind, role, provider, path string
}

var yamlKinds = []yamlPattern{
	{"eksctl.io/", "ClusterConfig", RoleControlPlane, "eks", "metadata.version"},
	{"eks.services.k8s.aws/", "Cluster", RoleControlPlane, "eks", "spec.version"},
	{"eks.services.k8s.aws/", "Nodegroup", RoleNodeGroup, "eks", "spec.version"},
	{"eks.aws.upbound.io/", "Cluster", RoleControlPlane, "eks", "spec.forProvider.version"},
	{"eks.aws.upbound.io/", "NodeGroup", RoleNodeGroup, "eks", "spec.forProvider.version"},
	{"container.gcp.upbound.io/", "Cluster", RoleControlPlane, "gke", "spec.forProvider.minMasterVersion"},
	{"containerservice.azure.upbound.io/", "KubernetesCluster", RoleControlPlane, "aks", "spec.forProvider.kubernetesVersion"},
}

// Discover scans files for Kubernetes version fields Jin knows how to manage.
func Discover(ws Workspace, files []string) []Candidate {
	var out []Candidate
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, ".tf"):
			out = append(out, discoverTerraform(ws, f)...)
		case strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml"):
			out = append(out, discoverYAML(ws, f)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Target.Role != out[j].Target.Role {
			return roleRank(out[i].Target.Role) < roleRank(out[j].Target.Role)
		}
		return out[i].Target.String() < out[j].Target.String()
	})
	return out
}

func roleRank(r string) int {
	switch r {
	case RoleControlPlane:
		return 0
	case RoleNodeGroup:
		return 1
	}
	return 2
}

func discoverTerraform(ws Workspace, file string) []Candidate {
	src, err := ws.Read(file)
	if err != nil {
		return nil
	}
	f, err := parseHCL(src, file)
	if err != nil {
		return nil
	}
	clusters := clusterNames(f.Body())
	var out []Candidate
	add := func(t Target, provider string, body *hclwrite.Body, nameAttrs ...string) {
		c := Candidate{Target: t, Provider: provider}
		if v, err := Current(ws, t); err != nil {
			c.Error = err.Error()
		} else {
			c.Current = v
		}
		for _, na := range nameAttrs {
			if a := body.GetAttribute(na); a != nil {
				toks := a.Expr().BuildTokens(nil)
				if s, ok := literalString(toks); ok && s != "" {
					c.Cluster = s
					break
				}
				// module.eks.cluster_name, aws_eks_cluster.this.name: the cluster block in this file.
				if s := clusters[blockRef(toks)]; s != "" {
					c.Cluster = s
					break
				}
			}
		}
		out = append(out, c)
	}
	for _, b := range f.Body().Blocks() {
		switch {
		case b.Type() == "resource" && len(b.Labels()) == 2:
			p, ok := tfResources[b.Labels()[0]]
			if !ok || b.Body().GetAttribute(p.attr) == nil {
				continue
			}
			t := Target{Role: p.role, Kind: KindTerraform, File: file, Address: b.Labels()[0] + "." + b.Labels()[1], Attribute: p.attr}
			if p.addonAttr != "" {
				if a := b.Body().GetAttribute(p.addonAttr); a != nil {
					t.Name, _ = literalString(a.Expr().BuildTokens(nil))
				}
			} else if p.role == RoleNodeGroup {
				t.Name = b.Labels()[1]
			}
			add(t, p.provider, b.Body(), p.nameAttr)
		case b.Type() == "module" && len(b.Labels()) == 1:
			src := b.Body().GetAttribute("source")
			if src == nil {
				continue
			}
			s, _ := literalString(src.Expr().BuildTokens(nil))
			for _, m := range tfModules {
				if !strings.HasPrefix(strings.TrimPrefix(s, "registry.terraform.io/"), m.source) {
					continue
				}
				for _, attr := range m.attrs {
					if b.Body().GetAttribute(attr) != nil {
						t := Target{Role: m.role, Kind: KindTerraform, File: file, Address: "module." + b.Labels()[0], Attribute: attr}
						if m.role == RoleNodeGroup {
							t.Name = b.Labels()[0]
						}
						add(t, m.provider, b.Body(), m.nameAttrs...)
						break
					}
				}
				break
			}
		}
	}
	return out
}

// clusterNames maps control-plane blocks in a file ("module.eks", "aws_eks_cluster.this") to their
// literal cluster names, so node groups and add-ons that reference them can be matched too.
func clusterNames(body *hclwrite.Body) map[string]string {
	out := map[string]string{}
	name := func(b *hclwrite.Body, attrs ...string) string {
		for _, a := range attrs {
			if at := b.GetAttribute(a); at != nil {
				if s, ok := literalString(at.Expr().BuildTokens(nil)); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	for _, b := range body.Blocks() {
		switch {
		case b.Type() == "resource" && len(b.Labels()) == 2:
			if p, ok := tfResources[b.Labels()[0]]; ok && p.role == RoleControlPlane {
				if n := name(b.Body(), p.nameAttr); n != "" {
					out[b.Labels()[0]+"."+b.Labels()[1]] = n
				}
			}
		case b.Type() == "module" && len(b.Labels()) == 1:
			src := b.Body().GetAttribute("source")
			if src == nil {
				continue
			}
			s, _ := literalString(src.Expr().BuildTokens(nil))
			for _, m := range tfModules {
				if m.role == RoleControlPlane && strings.HasPrefix(strings.TrimPrefix(s, "registry.terraform.io/"), m.source) {
					if n := name(b.Body(), m.nameAttrs...); n != "" {
						out["module."+b.Labels()[0]] = n
					}
					break
				}
			}
		}
	}
	return out
}

// blockRef returns "a.b" for a three-part traversal a.b.c, e.g. module.eks.cluster_name.
func blockRef(toks hclwrite.Tokens) string {
	t := significant(toks)
	if len(t) == 5 && t[0].Type == hclsyntax.TokenIdent && t[1].Type == hclsyntax.TokenDot && t[2].Type == hclsyntax.TokenIdent &&
		t[3].Type == hclsyntax.TokenDot && t[4].Type == hclsyntax.TokenIdent {
		return string(t[0].Bytes) + "." + string(t[2].Bytes)
	}
	return ""
}

func discoverYAML(ws Workspace, file string) []Candidate {
	src, err := ws.Read(file)
	if err != nil || len(src) > 1<<20 {
		return nil
	}
	var out []Candidate
	dec := yaml.NewDecoder(bytes.NewReader(src))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if !errors.Is(err, io.EOF) {
				return out
			}
			break
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		str := func(p string) string {
			n, err := walk(root, p)
			if err != nil || n.Kind != yaml.ScalarNode {
				return ""
			}
			return n.Value
		}
		api, kind, name := str("apiVersion"), str("kind"), str("metadata.name")
		for _, p := range yamlKinds {
			if !strings.HasPrefix(api, p.apiPrefix) || kind != p.kind {
				continue
			}
			v := str(p.path)
			if v == "" {
				continue
			}
			t := Target{Role: p.role, Kind: KindYAML, File: file, Path: p.path, Match: DocMatch{APIVersionPrefix: p.apiPrefix, Kind: kind, Name: name}}
			if p.role == RoleNodeGroup {
				t.Name = name
			}
			out = append(out, Candidate{Target: t, Current: v, Provider: p.provider, Cluster: name})
		}
	}
	return out
}

// memWorkspace is a Workspace over an in-memory file map, used for pending edits and tests.
type memWorkspace map[string][]byte

func (m memWorkspace) Read(file string) ([]byte, error) {
	b, ok := m[file]
	if !ok {
		return nil, fmt.Errorf("%s: file not found", file)
	}
	return b, nil
}

func (m memWorkspace) List(dir string) ([]string, error) {
	var out []string
	for f := range m {
		if path.Dir(f) == dir {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}
