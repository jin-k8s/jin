// Package aks upgrades Azure Kubernetes Service clusters through the Azure Resource Manager API.
package aks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

const apiVersion = "2024-09-01"

type NodeLister func(ctx context.Context) ([]inventory.Node, error)

// TokenSource returns an ARM bearer token.
type TokenSource func(ctx context.Context) (string, error)

type Executor struct {
	HTTP           *http.Client
	BaseURL        string // defaults to https://management.azure.com
	Token          TokenSource
	SubscriptionID string
	ResourceGroup  string
	Cluster        string
	Nodes          NodeLister
	Poll           time.Duration
}

func (x *Executor) Name() string { return "aks-direct" }

func (x *Executor) poll() time.Duration {
	if x.Poll > 0 {
		return x.Poll
	}
	return 20 * time.Second
}

func (x *Executor) clusterPath() string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s",
		x.SubscriptionID, x.ResourceGroup, x.Cluster)
}

func (x *Executor) do(ctx context.Context, method, p string, body, out any) error {
	base := x.BaseURL
	if base == "" {
		base = "https://management.azure.com"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+p+"?api-version="+apiVersion, rd)
	if err != nil {
		return err
	}
	tok, err := x.Token(ctx)
	if err != nil {
		return fmt.Errorf("azure credentials: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := x.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("AKS %s %s: %d %s %s", method, p, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type managedCluster struct {
	raw     map[string]any
	Version string
	State   string
}

type agentPool struct {
	Name    string
	Version string
	State   string
	raw     map[string]any
}

func str(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func (x *Executor) getCluster(ctx context.Context) (*managedCluster, error) {
	var raw map[string]any
	if err := x.do(ctx, http.MethodGet, x.clusterPath(), nil, &raw); err != nil {
		return nil, err
	}
	mc := &managedCluster{raw: raw, State: str(raw, "properties", "provisioningState")}
	mc.Version = str(raw, "properties", "currentKubernetesVersion")
	if mc.Version == "" {
		mc.Version = str(raw, "properties", "kubernetesVersion")
	}
	return mc, nil
}

func (x *Executor) listPools(ctx context.Context) ([]agentPool, error) {
	var r struct {
		Value []map[string]any `json:"value"`
	}
	if err := x.do(ctx, http.MethodGet, x.clusterPath()+"/agentPools", nil, &r); err != nil {
		return nil, err
	}
	var out []agentPool
	for _, p := range r.Value {
		v := str(p, "properties", "currentOrchestratorVersion")
		if v == "" {
			v = str(p, "properties", "orchestratorVersion")
		}
		name, _ := p["name"].(string)
		out = append(out, agentPool{Name: name, Version: v, State: str(p, "properties", "provisioningState"), raw: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// targetVersion picks the newest generally-available patch of minor `to` offered by AKS.
func (x *Executor) targetVersion(ctx context.Context, to kube.Version) (string, error) {
	var r struct {
		Properties struct {
			ControlPlaneProfile struct {
				Upgrades []struct {
					KubernetesVersion string `json:"kubernetesVersion"`
					IsPreview         bool   `json:"isPreview"`
				} `json:"upgrades"`
			} `json:"controlPlaneProfile"`
		} `json:"properties"`
	}
	if err := x.do(ctx, http.MethodGet, x.clusterPath()+"/upgradeProfiles/default", nil, &r); err != nil {
		return "", err
	}
	best := ""
	for _, u := range r.Properties.ControlPlaneProfile.Upgrades {
		v, err := kube.ParseVersion(u.KubernetesVersion)
		if err != nil || v != to || u.IsPreview {
			continue
		}
		if best == "" || comparePatch(u.KubernetesVersion, best) > 0 {
			best = u.KubernetesVersion
		}
	}
	if best == "" {
		return "", fmt.Errorf("AKS offers no generally available %s upgrade for this cluster", to)
	}
	return best, nil
}

func comparePatch(a, b string) int {
	var x, y [3]int
	_, _ = fmt.Sscanf(a, "%d.%d.%d", &x[0], &x[1], &x[2])
	_, _ = fmt.Sscanf(b, "%d.%d.%d", &y[0], &y[1], &y[2])
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func busy(state string) bool {
	switch state {
	case "Upgrading", "Updating", "Creating", "Scaling":
		return true
	}
	return false
}

func (x *Executor) ControlPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	mc, err := x.getCluster(ctx)
	if err != nil {
		return err
	}
	cur, err := kube.ParseVersion(mc.Version)
	if err != nil {
		return err
	}
	if cur == to {
		if busy(mc.State) {
			log.Info("Resuming: control plane is %s; waiting", strings.ToLower(mc.State))
			return x.waitCluster(ctx, "Control-plane upgrade", log, &to)
		}
		log.Info("Control plane already on %s (%s)", to, mc.Version)
		return nil
	}
	if busy(mc.State) {
		log.Info("Resuming: cluster is %s; waiting before upgrading", strings.ToLower(mc.State))
		if err := x.waitCluster(ctx, "Pending operation", log, nil); err != nil {
			return err
		}
		return x.ControlPlane(ctx, to, log)
	}
	if cur.Next() != to {
		return fmt.Errorf("control plane is on %s; upgrade one minor version at a time", cur)
	}
	full, err := x.targetVersion(ctx, to)
	if err != nil {
		return err
	}
	// A PUT that changes only properties.kubernetesVersion (node pools keep their
	// orchestratorVersion) is the control-plane-only upgrade `az aks upgrade --control-plane-only` performs.
	body := map[string]any{}
	for _, k := range []string{"location", "tags", "sku", "identity", "properties", "extendedLocation"} {
		if v, ok := mc.raw[k]; ok {
			body[k] = v
		}
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		return fmt.Errorf("unexpected managed cluster payload")
	}
	props["kubernetesVersion"] = full
	delete(props, "provisioningState")
	delete(props, "powerState")
	delete(props, "currentKubernetesVersion")
	if err := x.do(ctx, http.MethodPut, x.clusterPath(), body, nil); err != nil {
		return fmt.Errorf("start control-plane upgrade: %w", err)
	}
	log.Info("Started control-plane upgrade %s → %s. This step cannot be rolled back.", mc.Version, full)
	return x.waitCluster(ctx, "Control-plane upgrade", log, &to)
}

func (x *Executor) Addons(_ context.Context, to kube.Version, log upgrade.Logger) error {
	log.Info("AKS upgrades its managed add-ons with the control plane; update self-managed controllers for %s through your usual process", to)
	return nil
}

func (x *Executor) DataPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	mc, err := x.getCluster(ctx)
	if err != nil {
		return err
	}
	pools, err := x.listPools(ctx)
	if err != nil {
		return err
	}
	for _, p := range pools {
		v, err := kube.ParseVersion(p.Version)
		if err != nil {
			return fmt.Errorf("node pool %s: %w", p.Name, err)
		}
		progress := x.poolProgress(ctx, p.Name, to, log)
		if v == to && !busy(p.State) {
			log.Info("Node pool %s already on %s", p.Name, p.Version)
			continue
		}
		if !busy(p.State) {
			body := map[string]any{"properties": p.raw["properties"]}
			props, _ := body["properties"].(map[string]any)
			if props == nil {
				return fmt.Errorf("node pool %s: unexpected payload", p.Name)
			}
			props["orchestratorVersion"] = mc.Version
			delete(props, "provisioningState")
			delete(props, "currentOrchestratorVersion")
			delete(props, "powerState")
			if err := x.do(ctx, http.MethodPut, x.clusterPath()+"/agentPools/"+p.Name, body, nil); err != nil {
				return fmt.Errorf("node pool %s: %w", p.Name, err)
			}
			log.Info("Rolling node pool %s %s → %s (surge settings from the pool apply; drains respect PDBs)", p.Name, p.Version, mc.Version)
		} else {
			log.Info("Resuming: node pool %s is %s", p.Name, strings.ToLower(p.State))
		}
		if err := x.waitPool(ctx, p.Name, mc.Version, log, progress); err != nil {
			return err
		}
	}
	return nil
}

func (x *Executor) poolProgress(ctx context.Context, pool string, to kube.Version, log upgrade.Logger) func() {
	if x.Nodes == nil {
		return nil
	}
	last := -1
	return func() {
		nodes, err := x.Nodes(ctx)
		if err != nil {
			return
		}
		done, total := 0, 0
		for _, n := range nodes {
			if n.PoolType == inventory.PoolAKS && n.Pool == pool {
				total++
				if n.Version == to && n.Ready {
					done++
				}
			}
		}
		if total > 0 && done != last {
			last = done
			log.Progress(done, total, "nodes", "Node pool %s: %d/%d nodes Ready on %s", pool, done, total, to)
		}
	}
}

// waitCluster waits for provisioning to finish. With want set, "Succeeded" only counts once the
// reported version matches: right after a PUT, ARM can still return the previous state.
func (x *Executor) waitCluster(ctx context.Context, what string, log upgrade.Logger, want *kube.Version) error {
	return x.waitState(ctx, what, log, nil, func() (string, error) {
		mc, err := x.getCluster(ctx)
		if err != nil {
			return "", err
		}
		if v, _ := kube.ParseVersion(mc.Version); want != nil && mc.State == "Succeeded" && v != *want {
			return "Pending", nil
		}
		return mc.State, nil
	})
}

func (x *Executor) waitPool(ctx context.Context, pool, wantFull string, log upgrade.Logger, onTick func()) error {
	return x.waitState(ctx, "Node pool "+pool+" upgrade", log, onTick, func() (string, error) {
		var p map[string]any
		if err := x.do(ctx, http.MethodGet, x.clusterPath()+"/agentPools/"+pool, nil, &p); err != nil {
			return "", err
		}
		st := str(p, "properties", "provisioningState")
		cur := str(p, "properties", "currentOrchestratorVersion")
		if cur == "" {
			cur = str(p, "properties", "orchestratorVersion")
		}
		if st == "Succeeded" && wantFull != "" && cur != wantFull {
			return "Pending", nil
		}
		return st, nil
	})
}

func (x *Executor) waitState(ctx context.Context, what string, log upgrade.Logger, onTick func(), state func() (string, error)) error {
	start := time.Now()
	errs := 0
	for i := 0; ; i++ {
		s, err := state()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errs++; errs >= 3 {
				return err
			}
			log.Warn("Could not read provisioning state (attempt %d/3): %v", errs, err)
		case s == "Succeeded":
			if onTick != nil {
				onTick()
			}
			log.Info("%s completed in %s", what, time.Since(start).Round(time.Second))
			return nil
		case s == "Failed" || s == "Canceled":
			return fmt.Errorf("%s ended in state %s; check the AKS activity log", what, s)
		default:
			errs = 0
			if onTick != nil {
				onTick()
			}
			if i > 0 && i%6 == 0 {
				log.Info("%s in progress (%s elapsed, state %s)", what, time.Since(start).Round(time.Second), s)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}
