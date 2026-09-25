package app

import (
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestEKSFromKubeconfig(t *testing.T) {
	raw := &clientcmdapi.Config{
		Contexts: map[string]*clientcmdapi.Context{
			"arn:aws:eks:ap-south-1:111122223333:cluster/dev": {Cluster: "arn:aws:eks:ap-south-1:111122223333:cluster/dev", AuthInfo: "u1"},
			"prod":  {Cluster: "prod-cluster", AuthInfo: "u2"},
			"kind":  {Cluster: "kind-kind", AuthInfo: "u3"},
			"alias": {Cluster: "arn:aws:eks:eu-west-1:111122223333:cluster/payments", AuthInfo: "u3"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"u1": {Exec: &clientcmdapi.ExecConfig{Args: []string{"--region", "ap-south-1", "eks", "get-token", "--cluster-name", "dev", "--output", "json"}}},
			"u2": {Exec: &clientcmdapi.ExecConfig{
				Args: []string{"eks", "get-token", "--cluster-name", "prod-eks", "--region", "us-east-1", "--role-arn", "arn:aws:iam::1:role/jin"},
				Env:  []clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "prod-admin"}},
			}},
			"u3": {Token: "x"},
		},
	}

	dev := eksFromKubeconfig(raw, "arn:aws:eks:ap-south-1:111122223333:cluster/dev")
	if dev == nil || dev.Name != "dev" || dev.Region != "ap-south-1" {
		t.Fatalf("dev = %+v", dev)
	}
	prod := eksFromKubeconfig(raw, "prod")
	if prod == nil || prod.Name != "prod-eks" || prod.Region != "us-east-1" || prod.Profile != "prod-admin" || prod.RoleARN != "arn:aws:iam::1:role/jin" {
		t.Fatalf("prod = %+v", prod)
	}
	if alias := eksFromKubeconfig(raw, "alias"); alias == nil || alias.Name != "payments" || alias.Region != "eu-west-1" {
		t.Fatalf("alias = %+v", alias)
	}
	if k := eksFromKubeconfig(raw, "kind"); k != nil {
		t.Fatalf("kind should not be EKS: %+v", k)
	}
	if eksFromKubeconfig(raw, "missing") != nil {
		t.Fatal("missing context")
	}
}
