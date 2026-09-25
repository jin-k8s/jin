package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/jin-k8s/jin/internal/buildinfo"
	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/upgrade"
)

// EKSAPI is the part of the EKS API used to find and register clusters.
type EKSAPI interface {
	ListClusters(context.Context, *awseks.ListClustersInput, ...func(*awseks.Options)) (*awseks.ListClustersOutput, error)
	DescribeCluster(context.Context, *awseks.DescribeClusterInput, ...func(*awseks.Options)) (*awseks.DescribeClusterOutput, error)
}

// TokenFunc returns a Kubernetes bearer token and when to refresh it.
type TokenFunc func(ctx context.Context) (token string, refreshAt time.Time, err error)

// AWSClients builds the EKS API client and a Kubernetes token source for a cluster. Tests replace it.
var AWSClients = func(ctx context.Context, ref *upgrade.EKSRef) (EKSAPI, TokenFunc, error) {
	cfg, err := AWSConfig(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	return awseks.NewFromConfig(cfg), func(ctx context.Context) (string, time.Time, error) {
		t, err := EKSToken(ctx, cfg, ref.Name)
		// Tokens are valid for 15 minutes; refresh well before.
		return t, time.Now().Add(10 * time.Minute), err
	}, nil
}

// EKSToken produces the bearer token EKS accepts: a presigned STS GetCallerIdentity URL bound to the
// cluster name via the signed x-k8s-aws-id header (the same scheme `aws eks get-token` uses).
func EKSToken(ctx context.Context, cfg aws.Config, cluster string) (string, error) {
	pc := sts.NewPresignClient(sts.NewFromConfig(cfg))
	req, err := pc.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(so *sts.Options) {
			so.APIOptions = append(so.APIOptions,
				smithyhttp.SetHeaderValue("x-k8s-aws-id", cluster),
				smithyhttp.SetHeaderValue("X-Amz-Expires", "60"))
		})
	})
	if err != nil {
		return "", fmt.Errorf("sign EKS token: %w", err)
	}
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}

type tokenTransport struct {
	base      http.RoundTripper
	gen       TokenFunc
	mu        sync.Mutex
	token     string
	refreshAt time.Time
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.token == "" || time.Now().After(t.refreshAt) {
		tok, at, err := t.gen(r.Context())
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		t.token, t.refreshAt = tok, at
	}
	tok := t.token
	t.mu.Unlock()
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r)
}

func registeredRESTConfig(ctx context.Context, reg *clusters.Registration) (*rest.Config, error) {
	ref := reg.EKS
	_, gen, err := AWSClients(ctx, &ref)
	if err != nil {
		return nil, err
	}
	cfg := &rest.Config{
		Host:            reg.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{CAData: reg.CAData},
		UserAgent:       "jin/" + buildinfo.Version,
		QPS:             20,
		Burst:           40,
		Timeout:         60 * time.Second,
	}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return &tokenTransport{base: rt, gen: gen} }
	return cfg, nil
}

var (
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]?)?-[a-z]+-\d$`)
	profileRe = regexp.MustCompile(`^[A-Za-z0-9_.@+-]{1,64}$`)
	roleRe    = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)
)

// ValidateEKSRef rejects malformed input before it reaches AWS APIs.
func ValidateEKSRef(ref upgrade.EKSRef, needName bool) error {
	switch {
	case !regionRe.MatchString(ref.Region):
		return fmt.Errorf("invalid AWS region %q", ref.Region)
	case ref.Profile != "" && !profileRe.MatchString(ref.Profile):
		return fmt.Errorf("invalid AWS profile name %q", ref.Profile)
	case ref.RoleARN != "" && !roleRe.MatchString(ref.RoleARN):
		return fmt.Errorf("invalid IAM role ARN %q", ref.RoleARN)
	case needName && !nameRe.MatchString(ref.Name):
		return fmt.Errorf("invalid EKS cluster name %q", ref.Name)
	}
	return nil
}

// ListEKSClusters lists clusters visible to the given credentials in one region.
func (e *Env) ListEKSClusters(ctx context.Context, ref upgrade.EKSRef) ([]string, error) {
	if err := ValidateEKSRef(ref, false); err != nil {
		return nil, err
	}
	api, _, err := AWSClients(ctx, &ref)
	if err != nil {
		return nil, err
	}
	var names []string
	in := &awseks.ListClustersInput{}
	for {
		out, err := api.ListClusters(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list EKS clusters in %s: %w", ref.Region, err)
		}
		names = append(names, out.Clusters...)
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	sort.Strings(names)
	return names, nil
}

// RegisterEKS adds an EKS cluster by name and verifies Jin can reach its Kubernetes API.
func (e *Env) RegisterEKS(ctx context.Context, ref upgrade.EKSRef, by string) (*clusters.Registration, string, error) {
	if e.Settings == nil {
		return nil, "", errors.New("cluster settings are not available")
	}
	if err := ValidateEKSRef(ref, true); err != nil {
		return nil, "", err
	}
	api, _, err := AWSClients(ctx, &ref)
	if err != nil {
		return nil, "", err
	}
	out, err := api.DescribeCluster(ctx, &awseks.DescribeClusterInput{Name: aws.String(ref.Name)})
	if err != nil {
		return nil, "", fmt.Errorf("describe cluster %s in %s: %w", ref.Name, ref.Region, err)
	}
	c := out.Cluster
	if c.Endpoint == nil || c.CertificateAuthority == nil || c.CertificateAuthority.Data == nil {
		return nil, "", fmt.Errorf("cluster %s has no API endpoint yet (status %s)", ref.Name, c.Status)
	}
	ca, err := base64.StdEncoding.DecodeString(aws.ToString(c.CertificateAuthority.Data))
	if err != nil {
		return nil, "", fmt.Errorf("decode cluster CA: %w", err)
	}
	reg := &clusters.Registration{
		Context: clusters.EKSContext(ref.Region, ref.Name), Provider: "eks", EKS: ref,
		Endpoint: aws.ToString(c.Endpoint), CAData: ca, AddedBy: by, AddedAt: time.Now().UTC(),
	}

	cfg, err := registeredRESTConfig(ctx, reg)
	if err != nil {
		return nil, "", err
	}
	clients, err := clientsFor(reg.Context, cfg)
	if err != nil {
		return nil, "", err
	}
	sv, err := clients.Kube.Discovery().ServerVersion()
	if err != nil {
		if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
			return nil, "", fmt.Errorf("AWS accepted the credentials but the cluster did not: the IAM identity needs an EKS access entry. "+
				"Run: aws eks create-access-entry --cluster-name %s --region %s --principal-arn <jin-role-arn> && "+
				"aws eks associate-access-policy --cluster-name %s --region %s --principal-arn <jin-role-arn> "+
				"--policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy --access-scope type=cluster (%v)",
				ref.Name, ref.Region, ref.Name, ref.Region, err)
		}
		return nil, "", fmt.Errorf("cannot reach the Kubernetes API at %s (private endpoint, security group or network path?): %w", reg.Endpoint, err)
	}
	if err := e.Settings.Register(reg); err != nil {
		return nil, "", err
	}
	return reg, sv.GitVersion, nil
}

// AWSProfiles lists profile names from the shared AWS config and credentials files.
func AWSProfiles() []string {
	home, _ := os.UserHomeDir()
	files := []string{os.Getenv("AWS_CONFIG_FILE"), os.Getenv("AWS_SHARED_CREDENTIALS_FILE")}
	if files[0] == "" {
		files[0] = filepath.Join(home, ".aws", "config")
	}
	if files[1] == "" {
		files[1] = filepath.Join(home, ".aws", "credentials")
	}
	seen := map[string]bool{}
	var out []string
	for i, f := range files {
		// Paths come from the operator's AWS_* environment, as with the AWS SDK; never from requests.
		fh, err := os.Open(filepath.Clean(f)) //nolint:gosec // see above
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
				continue
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if i == 0 {
				if strings.HasPrefix(name, "sso-session ") || strings.HasPrefix(name, "services ") {
					continue
				}
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			}
			if name != "" && !seen[name] && profileRe.MatchString(name) {
				seen[name] = true
				out = append(out, name)
			}
		}
		_ = fh.Close()
	}
	sort.Strings(out)
	return out
}
