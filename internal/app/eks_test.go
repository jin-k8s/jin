package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/secrets"
	"github.com/jin-k8s/jin/internal/upgrade"
)

func TestEKSTokenFormat(t *testing.T) {
	cfg := aws.Config{Region: "ap-south-1", Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")}
	tok, err := EKSToken(context.Background(), cfg, "jin-test")
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := strings.CutPrefix(tok, "k8s-aws-v1.")
	if !ok || strings.Contains(raw, "=") {
		t.Fatalf("token prefix/padding: %s", tok)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "sts.ap-south-1.amazonaws.com" || q.Get("Action") != "GetCallerIdentity" {
		t.Fatalf("presigned URL: %s", u)
	}
	if !strings.Contains(q.Get("X-Amz-SignedHeaders"), "x-k8s-aws-id") {
		t.Fatalf("cluster name header must be signed: %s", q.Get("X-Amz-SignedHeaders"))
	}
}

type fakeEKS struct {
	endpoint string
	ca       []byte
}

func (f *fakeEKS) ListClusters(context.Context, *awseks.ListClustersInput, ...func(*awseks.Options)) (*awseks.ListClustersOutput, error) {
	return &awseks.ListClustersOutput{Clusters: []string{"zeta", "jin-test"}}, nil
}

func (f *fakeEKS) DescribeCluster(_ context.Context, in *awseks.DescribeClusterInput, _ ...func(*awseks.Options)) (*awseks.DescribeClusterOutput, error) {
	return &awseks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
		Name: in.Name, Endpoint: aws.String(f.endpoint), Status: ekstypes.ClusterStatusActive,
		CertificateAuthority: &ekstypes.Certificate{Data: aws.String(base64.StdEncoding.EncodeToString(f.ca))},
	}}, nil
}

// TestRegisterAndConnect runs registration against a TLS API server that only accepts the EKS token,
// then checks the cluster is listed and reachable without any kubeconfig entry.
func TestRegisterAndConnect(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k8s-aws-v1.test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401}`))
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.31.4-eks-abc", "major": "1", "minor": "31"})
	}))
	defer api.Close()
	ca := pemOf(t, api)

	token := "k8s-aws-v1.test"
	old := AWSClients
	t.Cleanup(func() { AWSClients = old })
	AWSClients = func(_ context.Context, ref *upgrade.EKSRef) (EKSAPI, TokenFunc, error) {
		return &fakeEKS{endpoint: api.URL, ca: ca}, func(context.Context) (string, time.Time, error) { return token, time.Now().Add(time.Hour), nil }, nil
	}

	dir := t.TempDir()
	e := &Env{Kubeconfig: filepath.Join(dir, "absent"), Settings: clusters.NewStore(filepath.Join(dir, "clusters.json"))}
	names, err := e.ListEKSClusters(context.Background(), upgrade.EKSRef{Region: "ap-south-1"})
	if err != nil || strings.Join(names, ",") != "jin-test,zeta" {
		t.Fatalf("list: %v %v", names, err)
	}
	if _, _, err := e.RegisterEKS(context.Background(), upgrade.EKSRef{Name: "bad name!", Region: "ap-south-1"}, "x"); err == nil {
		t.Fatal("invalid name must be rejected before calling AWS")
	}

	reg, version, err := e.RegisterEKS(context.Background(), upgrade.EKSRef{Name: "jin-test", Region: "ap-south-1", Profile: "jin-test"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Context != "eks:ap-south-1:jin-test" || version != "v1.31.4-eks-abc" {
		t.Fatalf("registration: %+v %s", reg, version)
	}

	ctxs, err := e.Contexts()
	if err != nil || len(ctxs) != 1 || !ctxs[0].Registered || ctxs[0].EKS.Name != "jin-test" || ctxs[0].EKS.Profile != "jin-test" {
		t.Fatalf("contexts: %+v %v", ctxs, err)
	}
	c, err := e.Connect(reg.Context)
	if err != nil {
		t.Fatal(err)
	}
	if sv, err := c.Kube.Discovery().ServerVersion(); err != nil || sv.GitVersion != "v1.31.4-eks-abc" || c.EKS == nil {
		t.Fatalf("connect: %v %v", sv, err)
	}

	token = "wrong"
	_, _, err = e.RegisterEKS(context.Background(), upgrade.EKSRef{Name: "other", Region: "ap-south-1"}, "alice")
	if err == nil || !strings.Contains(err.Error(), "create-access-entry") {
		t.Fatalf("unauthorized clusters must explain the access entry: %v", err)
	}

	if err := e.Settings.Unregister(reg.Context); err != nil {
		t.Fatal(err)
	}
	if ctxs, _ := e.Contexts(); len(ctxs) != 0 {
		t.Fatalf("unregister: %+v", ctxs)
	}
}

func TestGitHubTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	s, _ := secrets.Open(filepath.Join(dir, "s.json"), filepath.Join(dir, "s.key"))
	e := &Env{Secrets: s}
	t.Setenv("GITHUB_TOKEN", "from-env")
	t.Setenv("GITHUB_JIN_TOKEN", "from-custom-env")
	if tok, src := e.GitHubToken(nil); tok != "from-env" || src != "env:GITHUB_TOKEN" {
		t.Fatalf("env fallback: %s %s", tok, src)
	}
	_ = s.Put(GitHubSecret, "stored-token-0123456789", "a", nil)
	if tok, src := e.GitHubToken(nil); tok != "stored-token-0123456789" || src != "jin" {
		t.Fatalf("stored token must win over GITHUB_TOKEN: %s %s", tok, src)
	}
	if tok, _ := e.GitHubToken(&clusters.GitOps{TokenEnv: "GITHUB_JIN_TOKEN"}); tok != "from-custom-env" {
		t.Fatalf("explicit tokenEnv must win: %s", tok)
	}
}

func TestAWSProfiles(t *testing.T) {
	dir := t.TempDir()
	cfgFile, credFile := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	writeFile(t, cfgFile, "[default]\nregion=ap-south-1\n[profile jin-test]\nsso_session=x\n[sso-session x]\nsso_region=us-east-1\n")
	writeFile(t, credFile, "[default]\n[legacy]\n")
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credFile)
	if got := strings.Join(AWSProfiles(), ","); got != "default,jin-test,legacy" {
		t.Fatalf("profiles: %s", got)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pemOf(t *testing.T, s *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}
