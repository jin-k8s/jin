package gitops

import (
	"strings"
	"testing"
)

const eksTF = `module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.0" # module version, not Kubernetes

  cluster_name    = "payments-prod"
  cluster_version = "1.30" # bumped by Jin

  eks_managed_node_groups = {
    general = { instance_types = ["m6i.large"] }
  }
}

resource "aws_eks_addon" "coredns" {
  cluster_name  = module.eks.cluster_name
  addon_name    = "coredns"
  addon_version = "v1.11.1-eksbuild.9"
}

resource "aws_eks_node_group" "legacy" {
  cluster_name = "payments-prod"
  version      = var.node_version
}
`

func ws() memWorkspace {
	return memWorkspace{
		"infra/eks.tf":              []byte(eksTF),
		"infra/variables.tf":        []byte("variable \"node_version\" {\n  type    = string\n  default = \"1.29\"\n}\n\nvariable \"other\" {}\n"),
		"infra/terraform.tfvars":    []byte("region = \"ap-south-1\"\n"),
		"infra/prod.auto.tfvars":    []byte("# prod overrides\nnode_version = \"1.30\"\n"),
		"gke/main.tf":               []byte("locals {\n  k8s = \"1.30\"\n}\n\nresource \"google_container_cluster\" \"main\" {\n  name               = \"web\"\n  min_master_version = local.k8s\n}\n"),
		"gke/expr.tf":               []byte("resource \"google_container_node_pool\" \"np\" {\n  cluster = \"web\"\n  version = \"${local.k8s}.x\"\n}\n"),
		"clusters/prod-eksctl.yaml": []byte("---\n# eksctl config\napiVersion: eksctl.io/v1alpha5\nkind: ClusterConfig\nmetadata:\n  name: payments-prod   # keep\n  region: ap-south-1\n  version: \"1.30\" # kubernetes\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n"),
		"argo/app.yaml":             []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nspec:\n  sources:\n    - chart: karpenter\n      targetRevision: 1.0.6\n    - chart: cert-manager\n      targetRevision: 'v1.15.0'\n"),
	}
}

func TestTerraformLiteralPreservesFormatting(t *testing.T) {
	c, err := Apply(ws(), Target{Role: RoleControlPlane, Kind: KindTerraform, File: "infra/eks.tf", Address: "module.eks", Attribute: "cluster_version"}, "1.31")
	if err != nil {
		t.Fatal(err)
	}
	got := string(c.Files["infra/eks.tf"])
	if c.From != "1.30" || !strings.Contains(got, `cluster_version = "1.31" # bumped by Jin`) || !strings.Contains(got, `version = "~> 20.0" # module version`) {
		t.Fatalf("from %q, got:\n%s", c.From, got)
	}
	if strings.Count(got, "\n") != strings.Count(eksTF, "\n") {
		t.Fatal("line count changed: formatting not preserved")
	}
	if c, err := Apply(ws(), Target{Role: RoleControlPlane, Kind: KindTerraform, File: "infra/eks.tf", Address: "module.eks", Attribute: "cluster_version"}, "1.30"); err != nil || c != nil {
		t.Fatalf("no-op must return nil change: %v %v", c, err)
	}
}

func TestTerraformFollowsVariablesWithPrecedence(t *testing.T) {
	c, err := Apply(ws(), Target{Role: RoleNodeGroup, Kind: KindTerraform, File: "infra/eks.tf", Address: "aws_eks_node_group.legacy", Attribute: "version"}, "1.31")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Files["infra/prod.auto.tfvars"]; !ok || c.From != "1.30" || len(c.Files) != 1 {
		t.Fatalf("auto.tfvars wins over the variable default: %+v", c)
	}
	if !strings.Contains(string(c.Files["infra/prod.auto.tfvars"]), "# prod overrides\nnode_version = \"1.31\"") {
		t.Fatalf("%s", c.Files["infra/prod.auto.tfvars"])
	}

	w := ws()
	delete(w, "infra/prod.auto.tfvars")
	c, err = Apply(w, Target{Role: RoleNodeGroup, Kind: KindTerraform, File: "infra/eks.tf", Address: "aws_eks_node_group.legacy", Attribute: "version"}, "1.31")
	if err != nil || c.From != "1.29" || !strings.Contains(string(c.Files["infra/variables.tf"]), `default = "1.31"`) {
		t.Fatalf("variable default: %+v %v", c, err)
	}
}

func TestTerraformLocalsAndExpressions(t *testing.T) {
	c, err := Apply(ws(), Target{Role: RoleControlPlane, Kind: KindTerraform, File: "gke/main.tf", Address: "google_container_cluster.main", Attribute: "min_master_version"}, "1.31")
	if err != nil || !strings.Contains(string(c.Files["gke/main.tf"]), `k8s = "1.31"`) {
		t.Fatalf("local: %+v %v", c, err)
	}
	_, err = Apply(ws(), Target{Role: RoleNodeGroup, Kind: KindTerraform, File: "gke/expr.tf", Address: "google_container_node_pool.np", Attribute: "version"}, "1.31")
	if err == nil || !strings.Contains(err.Error(), "is an expression") {
		t.Fatalf("interpolations must be refused, got %v", err)
	}
	_, err = Apply(ws(), Target{Role: RoleControlPlane, Kind: KindTerraform, File: "infra/eks.tf", Address: "module.nope", Attribute: "cluster_version"}, "1.31")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing block: %v", err)
	}
}

func TestYAMLInPlace(t *testing.T) {
	tgt := Target{Role: RoleControlPlane, Kind: KindYAML, File: "clusters/prod-eksctl.yaml", Path: "metadata.version", Match: DocMatch{Kind: "ClusterConfig"}}
	c, err := Apply(ws(), tgt, "1.31")
	if err != nil {
		t.Fatal(err)
	}
	got := string(c.Files["clusters/prod-eksctl.yaml"])
	if c.From != "1.30" || !strings.Contains(got, "  version: \"1.31\" # kubernetes\n") || !strings.Contains(got, "name: payments-prod   # keep") {
		t.Fatalf("%s", got)
	}

	argo := Target{Role: RoleAddon, Name: "karpenter", Kind: KindYAML, File: "argo/app.yaml", Path: "spec.sources[chart=karpenter].targetRevision"}
	c, err = Apply(ws(), argo, "1.1.0")
	if err != nil || !strings.Contains(string(c.Files["argo/app.yaml"]), "targetRevision: 1.1.0\n    - chart: cert-manager") {
		t.Fatalf("selector edit: %v\n%s", err, c.Files["argo/app.yaml"])
	}
	single := Target{Role: RoleAddon, Name: "cert-manager", Kind: KindYAML, File: "argo/app.yaml", Path: "spec.sources[1].targetRevision", Format: "v{version}"}
	c, err = Apply(ws(), single, "1.16.1")
	if err != nil || !strings.Contains(string(c.Files["argo/app.yaml"]), "targetRevision: 'v1.16.1'") {
		t.Fatalf("single-quoted + format: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	w := ws()
	var files []string
	for f := range w {
		files = append(files, f)
	}
	cands := Discover(w, files)
	got := map[string]Candidate{}
	for _, c := range cands {
		got[c.Target.String()] = c
	}
	want := map[string]string{
		"infra/eks.tf: module.eks.cluster_version":                      "1.30",
		"infra/eks.tf: aws_eks_addon.coredns.addon_version":             "v1.11.1-eksbuild.9",
		"infra/eks.tf: aws_eks_node_group.legacy.version":               "1.30",
		"gke/main.tf: google_container_cluster.main.min_master_version": "1.30",
		"clusters/prod-eksctl.yaml: metadata.version":                   "1.30",
	}
	for k, v := range want {
		c, ok := got[k]
		if !ok || c.Current != v {
			t.Errorf("candidate %s: got %+v", k, c)
		}
	}
	if c := got["infra/eks.tf: module.eks.cluster_version"]; c.Cluster != "payments-prod" || c.Provider != "eks" {
		t.Errorf("cluster hint: %+v", c)
	}
	if c := got["infra/eks.tf: aws_eks_addon.coredns.addon_version"]; c.Target.Name != "coredns" || c.Target.Role != RoleAddon {
		t.Errorf("addon: %+v", c)
	}
	// cluster_name = module.eks.cluster_name resolves to the module's literal name.
	if c := got["infra/eks.tf: aws_eks_addon.coredns.addon_version"]; c.Cluster != "payments-prod" {
		t.Errorf("addon cluster via module reference: %q", c.Cluster)
	}
	if c := got["gke/expr.tf: google_container_node_pool.np.version"]; c.Error == "" {
		t.Errorf("expression candidates must carry an error: %+v", c)
	}
	if cands[0].Target.Role != RoleControlPlane {
		t.Error("control-plane candidates first")
	}
}
