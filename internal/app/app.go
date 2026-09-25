// Package app wires cluster access, planning and persistence for both the CLI and the server.
package app

import (
	"context"

	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	awseks "github.com/aws/aws-sdk-go-v2/service/eks"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/jin-k8s/jin/internal/buildinfo"
	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/compat"
	gkeexec "github.com/jin-k8s/jin/internal/executor/gke"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/runrecord"
	"github.com/jin-k8s/jin/internal/secrets"
	"github.com/jin-k8s/jin/internal/snapshot"
	"github.com/jin-k8s/jin/internal/support"
	"github.com/jin-k8s/jin/internal/upgrade"
)

// EKSCluster identifies an EKS cluster behind a kubeconfig context.
type EKSCluster struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Profile string `json:"profile,omitempty"`
	RoleARN string `json:"roleArn,omitempty"`
}

type Context struct {
	Name    string          `json:"name"`
	Cluster string          `json:"cluster"`
	Current bool            `json:"current"`
	EKS     *EKSCluster     `json:"eks,omitempty"`
	GKE     *upgrade.GKERef `json:"gke,omitempty"`
	AKS     *upgrade.AKSRef `json:"aks,omitempty"`
	// Environment and GitOps come from the cluster settings.
	Environment string `json:"environment,omitempty"`
	GitOps      bool   `json:"gitops"`
	// Registered clusters were added in Jin and are reached without a kubeconfig.
	Registered bool `json:"registered,omitempty"`
}

// Provider is the managed Kubernetes offering this context points at, when known.
func (c Context) Provider() string {
	switch {
	case c.EKS != nil:
		return "eks"
	case c.GKE != nil:
		return "gke"
	case c.AKS != nil:
		return "aks"
	}
	return ""
}

type Clients struct {
	Context  string
	Config   *rest.Config
	Kube     kubernetes.Interface
	Metadata metadata.Interface
	Dynamic  dynamic.Interface
	EKS      *EKSCluster
}

type Env struct {
	Kubeconfig string
	KB         *compat.KB
	Runs       *runrecord.Store
	// Settings is optional; the CLI planner works without it.
	Settings *clusters.Store
	// Secrets holds credentials entered in the UI (server only).
	Secrets *secrets.Store
}

// GitHubSecret is the secrets-store name of the GitHub token entered in the UI.
const GitHubSecret = "github"

// GitHubToken resolves the token for a repository: an explicit tokenEnv wins, then the token stored
// in Jin, then GITHUB_TOKEN.
func (e *Env) GitHubToken(g *clusters.GitOps) (token, source string) {
	if g != nil && g.TokenEnv != "" {
		return os.Getenv(g.TokenEnv), "env:" + g.TokenEnv
	}
	if e.Secrets != nil {
		if t, err := e.Secrets.Get(GitHubSecret); err == nil && t != "" {
			return t, "jin"
		}
	}
	return os.Getenv("GITHUB_TOKEN"), "env:GITHUB_TOKEN"
}

func (e *Env) loadingRules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if e.Kubeconfig != "" {
		rules.ExplicitPath = e.Kubeconfig
	}
	return rules
}

// Contexts lists kubeconfig contexts and clusters added in Jin. Credentials are never returned.
func (e *Env) Contexts() ([]Context, error) {
	raw, err := e.loadingRules().Load()
	if errors.Is(err, os.ErrNotExist) {
		// No kubeconfig is fine when clusters are added in Jin.
		raw, err = clientcmdapi.NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	out := make([]Context, 0, len(raw.Contexts))
	if e.Settings != nil {
		regs, err := e.Settings.Registrations()
		if err != nil {
			return nil, err
		}
		for _, r := range regs {
			ref := r.EKS
			c := Context{Name: r.Context, Cluster: r.Endpoint, Registered: true,
				EKS: &EKSCluster{Name: ref.Name, Region: ref.Region, Profile: ref.Profile, RoleARN: ref.RoleARN}}
			if st, err := e.Settings.Get(r.Context); err == nil {
				c.Environment = st.Environment
				c.GitOps = st.GitOps != nil && len(st.GitOps.Targets) > 0
			}
			out = append(out, c)
		}
	}
	for name, c := range raw.Contexts {
		ctx := Context{Name: name, Cluster: c.Cluster, Current: name == raw.CurrentContext, EKS: eksFromKubeconfig(raw, name)}
		if p, l, n, err := gkeexec.ParseContext(name); err == nil {
			ctx.GKE = &upgrade.GKERef{Project: p, Location: l, Name: n}
		}
		if e.Settings != nil {
			if st, err := e.Settings.Get(name); err == nil {
				ctx.Environment = st.Environment
				ctx.GitOps = st.GitOps != nil && len(st.GitOps.Targets) > 0
				if st.AKS != nil {
					ctx.AKS = st.AKS
				}
				if st.GKE != nil {
					ctx.GKE = st.GKE
				}
			}
		}
		out = append(out, ctx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Connect builds clients for a context; an empty name means the current context.
func (e *Env) Connect(contextName string) (*Clients, error) {
	if e.Settings != nil && contextName != "" {
		if reg, ok := e.Settings.Registration(contextName); ok {
			cfg, err := registeredRESTConfig(context.Background(), reg)
			if err != nil {
				return nil, err
			}
			c, err := clientsFor(reg.Context, cfg)
			if err != nil {
				return nil, err
			}
			ref := reg.EKS
			c.EKS = &EKSCluster{Name: ref.Name, Region: ref.Region, Profile: ref.Profile, RoleARN: ref.RoleARN}
			return c, nil
		}
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(e.loadingRules(), &clientcmd.ConfigOverrides{CurrentContext: contextName})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	name := contextName
	if name == "" {
		name = raw.CurrentContext
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, fmt.Errorf("kubeconfig context %q not found", name)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build client config for context %q: %w", name, err)
	}
	cfg.UserAgent = "jin/" + buildinfo.Version
	cfg.QPS = 20
	cfg.Burst = 40
	c, err := clientsFor(name, cfg)
	if err != nil {
		return nil, err
	}
	c.EKS = eksFromKubeconfig(&raw, name)
	return c, nil
}

func clientsFor(name string, cfg *rest.Config) (*Clients, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Context: name, Config: cfg, Kube: cs, Metadata: md, Dynamic: dyn}, nil
}

type PlanOptions struct {
	Target kube.Version
	// SkipSupport disables the provider support-calendar lookup (EKS: needs AWS credentials).
	SkipSupport bool
	Collect     inventory.Options
	Progress    func(step string)
	// Record persists the run record when true and a store is configured.
	Record bool
}

// Plan inspects the cluster and builds a plan. The returned record is always non-nil and
// reflects failures too; the error is the run error.
func (e *Env) Plan(ctx context.Context, c *Clients, opts PlanOptions) (*runrecord.Record, error) {
	rec := runrecord.New(runrecord.KindPlanRun, time.Now())
	rec.Cluster.Context = c.Context

	runErr := func() error {
		coll := &inventory.Collector{Client: c.Kube, Metadata: c.Metadata, KB: e.KB, Options: opts.Collect, Progress: opts.Progress}
		inv, err := coll.Collect(ctx)
		if err != nil {
			return err
		}
		rec.Cluster.Provider = inv.Provider
		rec.Cluster.Version = inv.ServerVersion
		rec.Cluster.GitVersion = inv.ServerGitVersion
		rec.Cluster.Nodes = len(inv.Nodes)
		rec.Cluster.HelmReleases = len(inv.HelmReleases)
		var cal *support.Calendar
		if c.EKS != nil && !opts.SkipSupport {
			if opts.Progress != nil {
				opts.Progress("EKS support calendar")
			}
			cal, err = e.eksCalendar(ctx, c.EKS)
			if err != nil {
				inv.Warnings = append(inv.Warnings, fmt.Sprintf("EKS support calendar: %v", err))
			}
		}
		p, err := plan.Build(inv, e.KB, plan.Options{Target: opts.Target, Calendar: cal})
		if err != nil {
			return err
		}
		rec.Plan = p
		return nil
	}()
	rec.Finish(time.Now(), runErr)

	if opts.Record && e.Runs != nil {
		if err := e.Runs.Save(rec); err != nil && runErr == nil {
			return rec, fmt.Errorf("save run record: %w", err)
		}
	}
	return rec, runErr
}

var eksARN = regexp.MustCompile(`^arn:aws[a-z-]*:eks:([a-z0-9-]+):\d{12}:cluster/(.+)$`)

// eksFromKubeconfig recognises contexts written by `aws eks update-kubeconfig` (exec plugin
// arguments) and ARN-named clusters/contexts.
func eksFromKubeconfig(raw *clientcmdapi.Config, ctxName string) *EKSCluster {
	kctx := raw.Contexts[ctxName]
	if kctx == nil {
		return nil
	}
	e := &EKSCluster{}
	if ai := raw.AuthInfos[kctx.AuthInfo]; ai != nil && ai.Exec != nil {
		args := ai.Exec.Args
		for i := 0; i < len(args)-1; i++ {
			switch args[i] {
			case "--cluster-name", "--cluster-id", "-i":
				e.Name = args[i+1]
			case "--region":
				e.Region = args[i+1]
			case "--profile":
				e.Profile = args[i+1]
			case "--role-arn", "--role", "-r":
				e.RoleARN = args[i+1]
			}
		}
		for _, env := range ai.Exec.Env {
			switch env.Name {
			case "AWS_PROFILE":
				e.Profile = env.Value
			case "AWS_REGION", "AWS_DEFAULT_REGION":
				if e.Region == "" {
					e.Region = env.Value
				}
			}
		}
	}
	for _, s := range []string{kctx.Cluster, ctxName} {
		if m := eksARN.FindStringSubmatch(s); m != nil {
			if e.Region == "" {
				e.Region = m[1]
			}
			if e.Name == "" {
				e.Name = m[2]
			}
		}
	}
	if e.Name == "" {
		return nil
	}
	return e
}

func (e *Env) eksCalendar(ctx context.Context, ref *EKSCluster) (*support.Calendar, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cfg, err := AWSConfig(ctx, EKSRef(ref))
	if err != nil {
		return nil, err
	}
	return support.EKSCalendar(ctx, awseks.NewFromConfig(cfg), time.Now())
}

// Snapshot captures workloads and cloud bindings for parity checks and migration assessments.
func (e *Env) Snapshot(ctx context.Context, c *Clients) (*snapshot.Snapshot, error) {
	return (&snapshot.Collector{Client: c.Kube, Dynamic: c.Dynamic, KB: e.KB, Context: c.Context}).Collect(ctx)
}
