// Package upgrade runs approved upgrades hop by hop with a human approval gate before each hop.
package upgrade

import (
	"context"
	"time"

	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/provider"
)

const (
	APIVersion  = "jin/v1alpha1"
	KindUpgrade = "Upgrade"

	// ModeDirect calls the cloud provider's APIs. It changes the cluster outside IaC, so
	// Terraform/GitOps sources must be updated afterwards to avoid drift.
	ModeDirect = "direct"
	// ModeGitOps raises a pull request per stage and waits for the team's pipeline to apply it.
	ModeGitOps = "gitops"
)

type Status string

const (
	StatusPending          Status = "pending"
	StatusAwaitingApproval Status = "awaiting-approval"
	StatusRunning          Status = "running"
	StatusSucceeded        Status = "succeeded"
	StatusFailed           Status = "failed"
	StatusSkipped          Status = "skipped"
	StatusCancelled        Status = "cancelled"
)

// Active upgrades hold the per-cluster lock; failed ones still need a retry or cancel decision.
func (s Status) Active() bool {
	return s == StatusAwaitingApproval || s == StatusRunning || s == StatusFailed
}

type StageName string

const (
	StagePreflight    StageName = "preflight"
	StageControlPlane StageName = "control-plane"
	StageAddons       StageName = "add-ons"
	StageDataPlane    StageName = "data-plane"
	StageVerify       StageName = "verify"
)

var stageOrder = []StageName{StagePreflight, StageControlPlane, StageAddons, StageDataPlane, StageVerify}

type Stage struct {
	Name       StageName  `json:"name"`
	Status     Status     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Message    string     `json:"message,omitempty"`
}

type Approval struct {
	By string `json:"by"`
	// Subject identifies the approver for counting distinct approvals (SSO subject or token user).
	Subject          string    `json:"subject,omitempty"`
	At               time.Time `json:"at"`
	Action           string    `json:"action"`
	Comment          string    `json:"comment,omitempty"`
	OverrideBlockers bool      `json:"overrideBlockers,omitempty"`
}

type Hop struct {
	Index     int                      `json:"index"`
	From      kube.Version             `json:"from"`
	To        kube.Version             `json:"to"`
	DataPlane provider.DataPlaneAction `json:"dataPlane"`
	Blockers  int                      `json:"blockers"`
	Status    Status                   `json:"status"`
	Stages    []Stage                  `json:"stages"`
	Approvals []Approval               `json:"approvals,omitempty"`
	// RequiredApprovals is set by the approval policy when the first approval arrives.
	RequiredApprovals int    `json:"requiredApprovals,omitempty"`
	Policy            string `json:"policy,omitempty"`
}

// overrideBlockers is true when any approver of this hop chose to override blockers.
func (h *Hop) overrideBlockers() bool {
	for _, a := range h.Approvals {
		if a.OverrideBlockers {
			return true
		}
	}
	return false
}

// Approvers returns the distinct subjects that approved this hop.
func (h *Hop) Approvers() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range h.Approvals {
		if a.Action != "approve" {
			continue
		}
		s := a.Subject
		if s == "" {
			s = a.By
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type EKSRef struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Profile string `json:"profile,omitempty"`
	RoleARN string `json:"roleArn,omitempty"`
}

type GKERef struct {
	Project  string `json:"project"`
	Location string `json:"location"`
	Name     string `json:"name"`
}

type AKSRef struct {
	SubscriptionID string `json:"subscriptionId"`
	ResourceGroup  string `json:"resourceGroup"`
	Name           string `json:"name"`
}

type ClusterRef struct {
	Context     string  `json:"context"`
	Provider    string  `json:"provider"`
	Environment string  `json:"environment,omitempty"`
	EKS         *EKSRef `json:"eks,omitempty"`
	GKE         *GKERef `json:"gke,omitempty"`
	AKS         *AKSRef `json:"aks,omitempty"`
}

type Upgrade struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	ID         string       `json:"id"`
	Mode       string       `json:"mode"`
	Cluster    ClusterRef   `json:"cluster"`
	PlanRunID  string       `json:"planRunId"`
	From       kube.Version `json:"from"`
	To         kube.Version `json:"to"`
	Status     Status       `json:"status"`
	CurrentHop int          `json:"currentHop"`
	Hops       []Hop        `json:"hops"`
	Error      string       `json:"error,omitempty"`
	CreatedBy  string       `json:"createdBy"`
	// CreatedBySubject identifies the creator for separation-of-duties checks.
	CreatedBySubject string     `json:"createdBySubject,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`
}

func (u *Upgrade) hop() *Hop { return &u.Hops[u.CurrentHop-1] }

type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
	// LevelState marks a state transition; clients should refetch the upgrade.
	LevelState Level = "state"
)

type Progress struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Unit  string `json:"unit"`
}

type Event struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Hop      int       `json:"hop,omitempty"`
	Stage    StageName `json:"stage,omitempty"`
	Level    Level     `json:"level"`
	Message  string    `json:"message"`
	Progress *Progress `json:"progress,omitempty"`
}

// Logger streams stage output to the upgrade's event log.
type Logger interface {
	Info(format string, args ...any)
	Warn(format string, args ...any)
	Progress(done, total int, unit, format string, args ...any)
}

// Executor changes the cluster. Every method must be idempotent: the engine re-runs an
// interrupted stage after a restart or a retry.
type Executor interface {
	Name() string
	ControlPlane(ctx context.Context, to kube.Version, log Logger) error
	Addons(ctx context.Context, to kube.Version, log Logger) error
	DataPlane(ctx context.Context, to kube.Version, log Logger) error
}

// Cluster is what the engine needs from one target cluster.
type Cluster interface {
	ServerVersion(ctx context.Context) (kube.Version, error)
	Plan(ctx context.Context, target kube.Version) (*plan.Plan, error)
	Verify(ctx context.Context, v kube.Version, log Logger) error
	Executor() Executor
}

type Connector func(ctx context.Context, u *Upgrade) (Cluster, error)

// StageTimeouter lets an executor override the default stage timeouts (GitOps waits for review).
type StageTimeouter interface {
	StageTimeout(StageName) time.Duration
}
