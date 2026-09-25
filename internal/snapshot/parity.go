package snapshot

import (
	"fmt"
	"sort"
	"strings"
)

type ParityStatus string

const (
	ParityMissing  ParityStatus = "missing"
	ParityNotReady ParityStatus = "not-ready"
	ParityMismatch ParityStatus = "mismatch"
	ParityAction   ParityStatus = "action"
	ParityOK       ParityStatus = "ok"
)

type ParityItem struct {
	Category string       `json:"category"`
	Name     string       `json:"name"`
	Status   ParityStatus `json:"status"`
	Source   string       `json:"source,omitempty"`
	Target   string       `json:"target,omitempty"`
	Detail   string       `json:"detail,omitempty"`
}

// Parity compares a live (blue) cluster with its replacement (green) before traffic moves.
type Parity struct {
	Source        string       `json:"source"`
	Target        string       `json:"target"`
	SourceVersion string       `json:"sourceVersion"`
	TargetVersion string       `json:"targetVersion"`
	Items         []ParityItem `json:"items"`
	Summary       struct {
		Missing  int `json:"missing"`
		NotReady int `json:"notReady"`
		Mismatch int `json:"mismatch"`
		Actions  int `json:"actions"`
		OK       int `json:"ok"`
	} `json:"summary"`
	// Ready is true when nothing is missing or unready on the target.
	Ready     bool     `json:"ready"`
	Checklist []string `json:"checklist"`
	Warnings  []string `json:"warnings,omitempty"`
}

func Compare(src, dst *Snapshot) *Parity {
	p := &Parity{Source: src.Context, Target: dst.Context, SourceVersion: src.ServerVersion.String(), TargetVersion: dst.ServerVersion.String()}
	add := func(it ParityItem) { p.Items = append(p.Items, it) }

	dstNS := set(dst.Namespaces)
	for _, ns := range src.Namespaces {
		if !dstNS[ns] {
			add(ParityItem{Category: "namespace", Name: ns, Status: ParityMissing})
		}
	}

	dstW := map[string]Workload{}
	for _, w := range dst.Workloads {
		dstW[w.ID()] = w
	}
	for _, w := range src.Workloads {
		t, ok := dstW[w.ID()]
		switch {
		case !ok:
			add(ParityItem{Category: "workload", Name: w.ID(), Status: ParityMissing, Source: fmt.Sprintf("%d/%d ready", w.Ready, w.Replicas)})
		case t.Replicas > 0 && t.Ready < t.Replicas:
			add(ParityItem{Category: "workload", Name: w.ID(), Status: ParityNotReady, Source: fmt.Sprintf("%d/%d ready", w.Ready, w.Replicas), Target: fmt.Sprintf("%d/%d ready", t.Ready, t.Replicas)})
		case strings.Join(w.Images, ",") != strings.Join(t.Images, ","):
			add(ParityItem{Category: "workload", Name: w.ID(), Status: ParityMismatch, Source: strings.Join(w.Images, ", "), Target: strings.Join(t.Images, ", "), Detail: "Different images: confirm the green cluster runs the intended release."})
		default:
			add(ParityItem{Category: "workload", Name: w.ID(), Status: ParityOK, Target: fmt.Sprintf("%d/%d ready", t.Ready, t.Replicas)})
		}
	}

	dstH := map[string]string{}
	for _, h := range dst.HelmReleases {
		dstH[h.Namespace+"/"+h.Name] = h.Chart + "@" + h.ChartVersion
	}
	for _, h := range src.HelmReleases {
		id, want := h.Namespace+"/"+h.Name, h.Chart+"@"+h.ChartVersion
		switch got, ok := dstH[id]; {
		case !ok:
			add(ParityItem{Category: "helm release", Name: id, Status: ParityMissing, Source: want})
		case got != want:
			add(ParityItem{Category: "helm release", Name: id, Status: ParityMismatch, Source: want, Target: got})
		}
	}

	dstCRD := set(dst.CRDs)
	for _, c := range src.CRDs {
		if !dstCRD[c] {
			add(ParityItem{Category: "CRD", Name: c, Status: ParityMissing, Detail: "Install the operator or chart that owns it before moving workloads that use it."})
		}
	}
	dstSC := map[string]string{}
	for _, sc := range dst.StorageClasses {
		dstSC[sc.Name] = sc.Provisioner
	}
	usedSC := map[string]bool{}
	for _, pvc := range src.PVCs {
		usedSC[pvc.StorageClass] = true
	}
	for _, sc := range src.StorageClasses {
		if _, ok := dstSC[sc.Name]; !ok && usedSC[sc.Name] {
			add(ParityItem{Category: "storage class", Name: sc.Name, Status: ParityMissing, Source: sc.Provisioner, Detail: "PVCs that reference it will stay Pending."})
		}
	}
	for name, ctrl := range src.IngressClasses {
		if _, ok := dst.IngressClasses[name]; !ok {
			add(ParityItem{Category: "ingress class", Name: name, Status: ParityMissing, Source: ctrl})
		}
	}
	dstSB := map[string]bool{}
	for _, b := range dst.SecretBackends {
		dstSB[b.Kind+" "+b.Namespace+"/"+b.Name] = true
	}
	for _, b := range src.SecretBackends {
		if !dstSB[b.Kind+" "+b.Namespace+"/"+b.Name] {
			add(ParityItem{Category: "secret backend", Name: b.Kind + " " + b.Namespace + "/" + b.Name, Status: ParityMissing, Source: b.Provider})
		}
	}

	var dataBytes int64
	for _, pvc := range src.PVCs {
		dataBytes += pvc.Bytes
	}
	if len(src.PVCs) > 0 {
		add(ParityItem{Category: "data", Name: fmt.Sprintf("%d persistent volume claim(s)", len(src.PVCs)), Status: ParityAction, Source: gib(dataBytes),
			Detail: "Volumes are not shared between clusters. Replicate data (Velero with volume snapshots or application-level replication) and plan a write freeze for cutover."})
	}
	external := len(src.Services) + len(src.Ingresses)
	if external > 0 {
		add(ParityItem{Category: "traffic", Name: fmt.Sprintf("%d external endpoint(s)", external), Status: ParityAction,
			Detail: "Load balancer addresses differ on the green cluster. Shift DNS gradually (weighted records) and keep the blue cluster serving until TTLs expire."})
	}

	for _, it := range p.Items {
		switch it.Status {
		case ParityMissing:
			p.Summary.Missing++
		case ParityNotReady:
			p.Summary.NotReady++
		case ParityMismatch:
			p.Summary.Mismatch++
		case ParityAction:
			p.Summary.Actions++
		default:
			p.Summary.OK++
		}
	}
	p.Ready = p.Summary.Missing == 0 && p.Summary.NotReady == 0
	sort.SliceStable(p.Items, func(i, j int) bool { return rank(p.Items[i].Status) < rank(p.Items[j].Status) })

	p.Checklist = []string{
		fmt.Sprintf("Green cluster %s runs Kubernetes %s; blue runs %s.", dst.Context, p.TargetVersion, p.SourceVersion),
		"Resolve every missing or not-ready item below, then re-run the comparison.",
		"Freeze deployments to the blue cluster, or point CI/CD and GitOps at both clusters.",
	}
	if len(src.PVCs) > 0 {
		p.Checklist = append(p.Checklist, fmt.Sprintf("Replicate %s of persistent data and verify it on green; schedule the write freeze.", gib(dataBytes)))
	}
	if external > 0 {
		p.Checklist = append(p.Checklist, fmt.Sprintf("Shift traffic for %d external endpoint(s) with weighted DNS (e.g. 10%% → 50%% → 100%%), watching error rates and latency.", external))
	}
	p.Checklist = append(p.Checklist,
		"Keep blue running and scaled until green has served full traffic through at least one peak period.",
		"Decommission blue through IaC once rollback is no longer needed.")
	p.Warnings = append(append([]string{}, src.Warnings...), dst.Warnings...)
	return p
}

func rank(s ParityStatus) int {
	switch s {
	case ParityMissing:
		return 0
	case ParityNotReady:
		return 1
	case ParityMismatch:
		return 2
	case ParityAction:
		return 3
	}
	return 4
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func gib(b int64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}
