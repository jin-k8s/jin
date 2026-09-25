package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/oauth2/google"

	aksexec "github.com/jin-k8s/jin/internal/executor/aks"
	eksexec "github.com/jin-k8s/jin/internal/executor/eks"
	gkeexec "github.com/jin-k8s/jin/internal/executor/gke"
	"github.com/jin-k8s/jin/internal/gitops"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/upgrade"
	"github.com/jin-k8s/jin/internal/verify"
)

// ClusterRef resolves the identity and settings of a context into what the engine records.
func (e *Env) ClusterRef(contextName, provider string) (upgrade.ClusterRef, error) {
	ref := upgrade.ClusterRef{Context: contextName, Provider: provider}
	ctxs, err := e.Contexts()
	if err != nil {
		return ref, err
	}
	for _, c := range ctxs {
		if c.Name != contextName {
			continue
		}
		ref.EKS = EKSRef(c.EKS)
		ref.GKE = c.GKE
		ref.AKS = c.AKS
		ref.Environment = c.Environment
	}
	return ref, nil
}

// Connector builds engine clusters: GitOps mode for any provider with a configured repository,
// direct mode through the EKS, GKE or AKS API.
func (e *Env) Connector() upgrade.Connector {
	return func(ctx context.Context, u *upgrade.Upgrade) (upgrade.Cluster, error) {
		clients, err := e.Connect(u.Cluster.Context)
		if err != nil {
			return nil, err
		}
		nodes := func(ctx context.Context) ([]inventory.Node, error) { return inventory.ListNodes(ctx, clients.Kube) }
		c := &cluster{env: e, clients: clients}

		switch u.Mode {
		case upgrade.ModeGitOps:
			x, err := e.gitopsExecutor(ctx, u, c)
			if err != nil {
				return nil, err
			}
			c.exec = x
		case upgrade.ModeDirect:
			switch {
			case u.Cluster.EKS != nil:
				x, err := eksExecutor(ctx, u.Cluster.EKS, nodes)
				if err != nil {
					return nil, err
				}
				c.exec = x
			case u.Cluster.GKE != nil:
				hc, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
				if err != nil {
					return nil, fmt.Errorf("google credentials: %w", err)
				}
				g := u.Cluster.GKE
				c.exec = &gkeexec.Executor{HTTP: hc, Project: g.Project, Location: g.Location, Cluster: g.Name, Nodes: gkeexec.NodeLister(nodes)}
			case u.Cluster.AKS != nil:
				cred, err := azidentity.NewDefaultAzureCredential(nil)
				if err != nil {
					return nil, fmt.Errorf("azure credentials: %w", err)
				}
				a := u.Cluster.AKS
				c.exec = &aksexec.Executor{
					HTTP: &http.Client{Timeout: 60 * time.Second},
					Token: func(ctx context.Context) (string, error) {
						t, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
						return t.Token, err
					},
					SubscriptionID: a.SubscriptionID, ResourceGroup: a.ResourceGroup, Cluster: a.Name, Nodes: aksexec.NodeLister(nodes),
				}
			default:
				return nil, errors.New("direct mode needs the cloud identity of the cluster: EKS and GKE are detected from the kubeconfig, AKS needs subscription and resource group in the cluster settings")
			}
		default:
			return nil, fmt.Errorf("unsupported upgrade mode %q", u.Mode)
		}
		return c, nil
	}
}

func (e *Env) gitopsExecutor(ctx context.Context, u *upgrade.Upgrade, c *cluster) (*gitops.Executor, error) {
	if e.Settings == nil {
		return nil, errors.New("cluster settings are not available")
	}
	st, err := e.Settings.Get(u.Cluster.Context)
	if err != nil {
		return nil, err
	}
	g := st.GitOps
	if g == nil || len(g.Targets) == 0 {
		return nil, errors.New("GitOps mode needs a repository and at least one version target in the cluster settings")
	}
	token, source := e.GitHubToken(g)
	if token == "" {
		return nil, fmt.Errorf("GitOps mode needs a GitHub token: add one under Integrations in the UI, or set %s on the Jin server", strings.TrimPrefix(source, "env:"))
	}
	name := u.Cluster.Context
	x := &gitops.Executor{
		Repo:        &gitops.GitHub{BaseURL: g.BaseURL, Owner: g.Owner, Name: g.Repo, Token: token},
		BaseBranch:  g.BaseBranch,
		Targets:     g.Targets,
		Cluster:     c,
		UpgradeID:   u.ID,
		ClusterName: name,
	}
	if g.ApplyTimeoutMinutes > 0 {
		x.ApplyTimeout = time.Duration(g.ApplyTimeoutMinutes) * time.Minute
	}
	if u.Cluster.EKS != nil {
		x.ClusterName = u.Cluster.EKS.Name
		if eksX, err := eksExecutor(ctx, u.Cluster.EKS, nil); err == nil {
			x.AddonCatalog = gitops.AddonVersions{Desired: eksX.DefaultAddonVersion, Running: eksX.RunningAddonVersion}
		}
	}
	return x, nil
}

func eksExecutor(ctx context.Context, ref *upgrade.EKSRef, nodes func(context.Context) ([]inventory.Node, error)) (*eksexec.Executor, error) {
	cfg, err := AWSConfig(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &eksexec.Executor{API: awseks.NewFromConfig(cfg), Cluster: ref.Name, Nodes: nodes}, nil
}

// AWSConfig mirrors the credentials the kubeconfig exec plugin uses: profile, region and role.
func AWSConfig(ctx context.Context, ref *upgrade.EKSRef) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if ref.Region != "" {
		opts = append(opts, config.WithRegion(ref.Region))
	}
	if ref.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(ref.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS configuration: %w", err)
	}
	if ref.RoleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), ref.RoleARN,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "jin" }))
	}
	return cfg, nil
}

func EKSRef(c *EKSCluster) *upgrade.EKSRef {
	if c == nil {
		return nil
	}
	return &upgrade.EKSRef{Name: c.Name, Region: c.Region, Profile: c.Profile, RoleARN: c.RoleARN}
}

type cluster struct {
	env     *Env
	clients *Clients
	exec    upgrade.Executor
}

func (c *cluster) ServerVersion(context.Context) (kube.Version, error) {
	sv, err := c.clients.Kube.Discovery().ServerVersion()
	if err != nil {
		return kube.Version{}, err
	}
	return kube.ParseVersion(sv.GitVersion)
}

func (c *cluster) Nodes(ctx context.Context) ([]inventory.Node, error) {
	return inventory.ListNodes(ctx, c.clients.Kube)
}

func (c *cluster) Plan(ctx context.Context, target kube.Version) (*plan.Plan, error) {
	rec, err := c.env.Plan(ctx, c.clients, PlanOptions{Target: target, SkipSupport: true})
	if err != nil {
		return nil, err
	}
	return rec.Plan, nil
}

func (c *cluster) Verify(ctx context.Context, v kube.Version, log upgrade.Logger) error {
	return verify.Cluster(ctx, c.clients.Kube, v, log, verify.Options{})
}

func (c *cluster) Executor() upgrade.Executor { return c.exec }
