// Package gke upgrades Google Kubernetes Engine clusters through the GKE REST API (v1).
package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

type NodeLister func(ctx context.Context) ([]inventory.Node, error)

// Executor needs an HTTP client that adds Google credentials (see NewHTTPClient).
type Executor struct {
	HTTP     *http.Client
	BaseURL  string // defaults to https://container.googleapis.com/v1
	Project  string
	Location string
	Cluster  string
	Nodes    NodeLister
	Poll     time.Duration
}

func (x *Executor) Name() string { return "gke-direct" }

func (x *Executor) base() string {
	if x.BaseURL != "" {
		return strings.TrimRight(x.BaseURL, "/")
	}
	return "https://container.googleapis.com/v1"
}

func (x *Executor) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", x.Project, x.Location)
}

func (x *Executor) clusterPath() string { return x.parent() + "/clusters/" + x.Cluster }

func (x *Executor) poll() time.Duration {
	if x.Poll > 0 {
		return x.Poll
	}
	return 20 * time.Second
}

type nodePool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Config  struct {
		ImageType string `json:"imageType"`
	} `json:"config"`
}

type cluster struct {
	CurrentMasterVersion string     `json:"currentMasterVersion"`
	Status               string     `json:"status"`
	NodePools            []nodePool `json:"nodePools"`
}

type operation struct {
	Name          string `json:"name"`
	OperationType string `json:"operationType"`
	Status        string `json:"status"`
	TargetLink    string `json:"targetLink"`
	StatusMessage string `json:"statusMessage"`
	Error         *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (x *Executor) do(ctx context.Context, method, p string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.base()+"/"+p, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("GKE %s %s: %d %s", method, p, resp.StatusCode, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (x *Executor) get(ctx context.Context) (*cluster, error) {
	var c cluster
	return &c, x.do(ctx, http.MethodGet, x.clusterPath(), nil, &c)
}

func (x *Executor) ControlPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	c, err := x.get(ctx)
	if err != nil {
		return err
	}
	cur, err := kube.ParseVersion(c.CurrentMasterVersion)
	if err != nil {
		return err
	}
	if cur == to {
		log.Info("Control plane already on %s (%s)", to, c.CurrentMasterVersion)
		return nil
	}
	if op, err := x.runningOp(ctx, "UPGRADE_MASTER", ""); err != nil {
		return err
	} else if op != nil {
		log.Info("Resuming: waiting for control-plane operation %s already in progress", op.Name)
		return x.wait(ctx, op.Name, "Control-plane upgrade", log, nil)
	}
	if cur.Next() != to {
		return fmt.Errorf("control plane is on %s; upgrade one minor version at a time", cur)
	}
	var op operation
	// GKE resolves the "1.X" alias to the newest patch available to the cluster.
	if err := x.do(ctx, http.MethodPost, x.clusterPath()+":updateMaster", map[string]string{"masterVersion": to.String()}, &op); err != nil {
		return fmt.Errorf("start control-plane upgrade: %w", err)
	}
	log.Info("Started control-plane upgrade %s → %s (operation %s). This step cannot be rolled back.", c.CurrentMasterVersion, to, op.Name)
	return x.wait(ctx, op.Name, "Control-plane upgrade", log, nil)
}

func (x *Executor) Addons(_ context.Context, to kube.Version, log upgrade.Logger) error {
	log.Info("GKE manages kube-proxy, DNS and the CNI with the control plane; update self-managed controllers for %s through your usual process", to)
	return nil
}

func (x *Executor) DataPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	c, err := x.get(ctx)
	if err != nil {
		return err
	}
	for _, np := range c.NodePools {
		v, err := kube.ParseVersion(np.Version)
		if err != nil {
			return fmt.Errorf("node pool %s: %w", np.Name, err)
		}
		if v == to {
			log.Info("Node pool %s already on %s", np.Name, to)
			continue
		}
		progress := x.poolProgress(ctx, np.Name, to, log)
		if op, err := x.runningOp(ctx, "UPGRADE_NODES", "/nodePools/"+np.Name); err != nil {
			return err
		} else if op != nil {
			log.Info("Resuming: waiting for node pool %s operation %s", np.Name, op.Name)
			if err := x.wait(ctx, op.Name, "Node pool "+np.Name+" upgrade", log, progress); err != nil {
				return err
			}
			continue
		}
		var op operation
		// "-" upgrades the pool to the control-plane version.
		body := map[string]string{"nodeVersion": "-", "imageType": np.Config.ImageType}
		if err := x.do(ctx, http.MethodPut, x.clusterPath()+"/nodePools/"+np.Name, body, &op); err != nil {
			return fmt.Errorf("node pool %s: %w", np.Name, err)
		}
		log.Info("Rolling node pool %s %s → %s (operation %s); GKE respects PDBs for up to one hour per node", np.Name, np.Version, to, op.Name)
		if err := x.wait(ctx, op.Name, "Node pool "+np.Name+" upgrade", log, progress); err != nil {
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
			if n.PoolType == inventory.PoolGKE && n.Pool == pool {
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

func (x *Executor) runningOp(ctx context.Context, typ, suffix string) (*operation, error) {
	var r struct {
		Operations []operation `json:"operations"`
	}
	if err := x.do(ctx, http.MethodGet, x.parent()+"/operations", nil, &r); err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	for i, op := range r.Operations {
		if op.OperationType == typ && op.Status != "DONE" && strings.HasSuffix(op.TargetLink, "/clusters/"+x.Cluster+suffix) {
			return &r.Operations[i], nil
		}
	}
	return nil, nil
}

var opName = regexp.MustCompile(`[^/]+$`)

func (x *Executor) wait(ctx context.Context, name, what string, log upgrade.Logger, onTick func()) error {
	name = opName.FindString(name)
	start := time.Now()
	errs := 0
	for i := 0; ; i++ {
		var op operation
		err := x.do(ctx, http.MethodGet, x.parent()+"/operations/"+name, nil, &op)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errs++; errs >= 3 {
				return err
			}
			log.Warn("Could not read operation %s (attempt %d/3): %v", name, errs, err)
		case op.Status == "DONE":
			if onTick != nil {
				onTick()
			}
			if op.Error != nil && op.Error.Message != "" {
				return fmt.Errorf("%s failed: %s", what, op.Error.Message)
			}
			log.Info("%s completed in %s", what, time.Since(start).Round(time.Second))
			return nil
		default:
			errs = 0
			if onTick != nil {
				onTick()
			}
			if i > 0 && i%6 == 0 {
				msg := op.StatusMessage
				if msg == "" {
					msg = strings.ToLower(op.Status)
				}
				log.Info("%s in progress (%s elapsed): %s", what, time.Since(start).Round(time.Second), msg)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}

var contextRe = regexp.MustCompile(`^gke_([a-z][a-z0-9-]{4,28}[a-z0-9])_([a-z0-9-]+)_([a-z0-9-]+)$`)

// ParseContext recognises contexts written by `gcloud container clusters get-credentials`.
func ParseContext(name string) (project, location, cluster string, err error) {
	m := contextRe.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", errors.New("not a gcloud-generated GKE context (gke_PROJECT_LOCATION_CLUSTER)")
	}
	return m[1], m[2], m[3], nil
}
