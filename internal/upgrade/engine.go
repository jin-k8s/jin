package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/runrecord"
)

// StageTimeouts bound each stage. Data-plane rolls of large node groups dominate.
var StageTimeouts = map[StageName]time.Duration{
	StagePreflight:    10 * time.Minute,
	StageControlPlane: 90 * time.Minute,
	StageAddons:       60 * time.Minute,
	StageDataPlane:    6 * time.Hour,
	StageVerify:       30 * time.Minute,
}

type Engine struct {
	store   *Store
	connect Connector
	now     func() time.Time

	mu       sync.Mutex // guards upgrade documents and the maps below
	base     context.Context
	stop     context.CancelFunc
	running  map[string]context.CancelFunc
	cancelBy map[string]string
	wg       sync.WaitGroup

	evMu sync.Mutex // guards event sequencing and subscribers
	seq  map[string]int64
	subs map[string]map[chan Event]struct{}
}

func NewEngine(store *Store, connect Connector) *Engine {
	base, stop := context.WithCancel(context.Background())
	return &Engine{
		store: store, connect: connect, now: time.Now,
		base: base, stop: stop,
		running: map[string]context.CancelFunc{}, cancelBy: map[string]string{},
		seq: map[string]int64{}, subs: map[string]map[chan Event]struct{}{},
	}
}

// Resume restarts upgrades that were running when the process stopped. Executors are
// idempotent, so the interrupted stage simply runs again.
func (e *Engine) Resume() error {
	ups, err := e.store.List()
	if err != nil {
		return err
	}
	for _, u := range ups {
		if u.Status == StatusRunning {
			e.emit(u.ID, Event{Hop: u.CurrentHop, Level: LevelWarn, Message: "Jin restarted; resuming the interrupted stage"})
			e.start(u.ID)
		}
	}
	return nil
}

// Shutdown stops workers without changing upgrade state; Resume picks them up again.
func (e *Engine) Shutdown() {
	e.stop()
	e.wg.Wait()
}

func (e *Engine) Get(id string) (*Upgrade, error) { return e.store.Get(id) }

func (e *Engine) List() ([]*Upgrade, error) { return e.store.List() }

func (e *Engine) Events(id string, after int64) ([]Event, error) { return e.store.Events(id, after) }

type CreateRequest struct {
	Run     *runrecord.Record
	Mode    string
	Cluster ClusterRef
	By      string
	Subject string
}

func (e *Engine) Create(ctx context.Context, req CreateRequest) (*Upgrade, error) {
	r := req.Run
	if r == nil || r.Status != runrecord.StatusSucceeded || r.Plan == nil {
		return nil, fmt.Errorf("upgrades start from a successful plan run: %w", ErrConflict)
	}
	if req.Mode != ModeDirect && req.Mode != ModeGitOps {
		return nil, fmt.Errorf("unsupported mode %q", req.Mode)
	}
	now := e.now().UTC()
	u := &Upgrade{
		APIVersion: APIVersion, Kind: KindUpgrade, ID: newID(now), Mode: req.Mode,
		Cluster: req.Cluster, PlanRunID: r.ID, From: r.Plan.Current, To: r.Plan.Target,
		Status: StatusAwaitingApproval, CurrentHop: 1, CreatedBy: req.By, CreatedBySubject: req.Subject, CreatedAt: now, UpdatedAt: now,
	}
	for i, h := range r.Plan.Hops {
		hop := Hop{Index: i + 1, From: h.From, To: h.To, DataPlane: h.DataPlane, Status: StatusPending}
		for _, f := range h.Findings {
			if f.Severity == check.SeverityBlocker {
				hop.Blockers++
			}
		}
		for _, s := range stageOrder {
			hop.Stages = append(hop.Stages, Stage{Name: s, Status: StatusPending})
		}
		u.Hops = append(u.Hops, hop)
	}
	u.Hops[0].Status = StatusAwaitingApproval

	// Validate cluster access and executor support before anything is persisted.
	if _, err := e.connect(ctx, u); err != nil {
		return nil, err
	}

	e.mu.Lock()
	all, err := e.store.List()
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	for _, o := range all {
		if o.Cluster.Context == u.Cluster.Context && o.Status.Active() {
			e.mu.Unlock()
			return nil, fmt.Errorf("upgrade %s is already %s for this cluster; finish or cancel it first: %w", o.ID, o.Status, ErrConflict)
		}
	}
	err = e.store.Save(u)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}

	e.emit(u.ID, Event{Level: LevelState, Message: fmt.Sprintf("Upgrade %s → %s created by %s from plan %s (%s mode)", u.From, u.To, req.By, r.ID, req.Mode)})
	e.emitApprovalNeeded(u)
	return u, nil
}

type ApproveRequest struct {
	Hop              int
	By               string
	Subject          string
	Comment          string
	OverrideBlockers bool
	// Required is the number of distinct approvers the policy demands (minimum 1).
	Required int
	Policy   string
}

func (e *Engine) Approve(id string, req ApproveRequest) (*Upgrade, error) {
	if req.Subject == "" {
		req.Subject = req.By
	}
	if req.Required < 1 {
		req.Required = 1
	}
	var complete bool
	u, err := e.mutate(id, func(u *Upgrade) error {
		if u.Status != StatusAwaitingApproval {
			return fmt.Errorf("upgrade is %s, not awaiting approval: %w", u.Status, ErrConflict)
		}
		if req.Hop != u.CurrentHop {
			return fmt.Errorf("hop %d is awaiting approval, not hop %d: %w", u.CurrentHop, req.Hop, ErrConflict)
		}
		h := u.hop()
		for _, s := range h.Approvers() {
			if s == req.Subject {
				return fmt.Errorf("%s already approved hop %d: %w", req.By, h.Index, ErrConflict)
			}
		}
		h.Approvals = append(h.Approvals, Approval{By: req.By, Subject: req.Subject, At: e.now().UTC(), Action: "approve", Comment: req.Comment, OverrideBlockers: req.OverrideBlockers})
		h.RequiredApprovals, h.Policy = req.Required, req.Policy
		if len(h.Approvers()) >= req.Required {
			complete = true
			h.Status = StatusRunning
			u.Status = StatusRunning
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	h := u.hop()
	msg := fmt.Sprintf("Hop %d (%s → %s) approved by %s", h.Index, h.From, h.To, req.By)
	if req.Required > 1 {
		msg += fmt.Sprintf(" (%d of %d)", len(h.Approvers()), req.Required)
	}
	if req.OverrideBlockers {
		msg += " with blocker override"
	}
	if req.Comment != "" {
		msg += ": " + req.Comment
	}
	e.emit(id, Event{Hop: h.Index, Level: LevelState, Message: msg})
	if complete {
		e.start(id)
	} else {
		e.emit(id, Event{Hop: h.Index, Level: LevelState, Message: fmt.Sprintf("Waiting for %d more approval(s) under policy %q", req.Required-len(h.Approvers()), req.Policy)})
	}
	return u, nil
}

func (e *Engine) Retry(id, by, comment string) (*Upgrade, error) {
	u, err := e.mutate(id, func(u *Upgrade) error {
		if u.Status != StatusFailed {
			return fmt.Errorf("only failed upgrades can be retried (status %s): %w", u.Status, ErrConflict)
		}
		h := u.hop()
		prev := false
		if n := len(h.Approvals); n > 0 {
			prev = h.Approvals[n-1].OverrideBlockers
		}
		h.Approvals = append(h.Approvals, Approval{By: by, Subject: by, At: e.now().UTC(), Action: "retry", Comment: comment, OverrideBlockers: prev})
		for i := range h.Stages {
			if h.Stages[i].Status == StatusFailed {
				h.Stages[i].Status = StatusPending
				h.Stages[i].Message = ""
			}
		}
		h.Status = StatusRunning
		u.Status = StatusRunning
		u.Error = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.emit(id, Event{Hop: u.CurrentHop, Level: LevelState, Message: fmt.Sprintf("Retry of hop %d requested by %s", u.CurrentHop, by)})
	e.start(id)
	return u, nil
}

func (e *Engine) Cancel(id, by string) (*Upgrade, error) {
	e.mu.Lock()
	u, err := e.store.Get(id)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	switch u.Status {
	case StatusRunning:
		if c := e.running[id]; c != nil {
			e.cancelBy[id] = by
			c()
			e.mu.Unlock()
			e.emit(id, Event{Hop: u.CurrentHop, Level: LevelWarn, Message: fmt.Sprintf("Cancellation requested by %s; stopping after the current API call", by)})
			return u, nil
		}
		fallthrough
	case StatusAwaitingApproval, StatusFailed:
		e.finishLocked(u, StatusCancelled, "")
		u.hop().Status = StatusCancelled
		err := e.store.Save(u)
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		e.emit(id, Event{Hop: u.CurrentHop, Level: LevelState, Message: "Upgrade cancelled by " + by})
		return u, nil
	}
	e.mu.Unlock()
	return nil, fmt.Errorf("upgrade is already %s: %w", u.Status, ErrConflict)
}

// Subscribe streams new events for an upgrade. The channel is closed if the consumer falls behind;
// consumers should re-sync with Events(id, lastSeq).
func (e *Engine) Subscribe(id string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	e.evMu.Lock()
	if e.subs[id] == nil {
		e.subs[id] = map[chan Event]struct{}{}
	}
	e.subs[id][ch] = struct{}{}
	e.evMu.Unlock()
	return ch, func() {
		e.evMu.Lock()
		if _, ok := e.subs[id][ch]; ok {
			delete(e.subs[id], ch)
			close(ch)
		}
		e.evMu.Unlock()
	}
}

func (e *Engine) mutate(id string, fn func(u *Upgrade) error) (*Upgrade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, err := e.store.Get(id)
	if err != nil {
		return nil, err
	}
	if err := fn(u); err != nil {
		return nil, err
	}
	u.UpdatedAt = e.now().UTC()
	if err := e.store.Save(u); err != nil {
		return nil, err
	}
	return u, nil
}

func (e *Engine) finishLocked(u *Upgrade, s Status, errMsg string) {
	now := e.now().UTC()
	u.Status = s
	u.Error = errMsg
	u.UpdatedAt = now
	if s == StatusSucceeded || s == StatusCancelled {
		u.FinishedAt = &now
	}
}

func (e *Engine) start(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, busy := e.running[id]; busy {
		return
	}
	ctx, cancel := context.WithCancel(e.base)
	e.running[id] = cancel
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.running, id)
			delete(e.cancelBy, id)
			e.mu.Unlock()
			cancel()
		}()
		e.run(ctx, id)
	}()
}

func (e *Engine) run(ctx context.Context, id string) {
	u, err := e.store.Get(id)
	if err != nil || u.Status != StatusRunning {
		return
	}
	hopIdx := u.CurrentHop
	cluster, err := e.connect(ctx, u)
	if err != nil {
		e.failStage(ctx, id, hopIdx, 0, fmt.Errorf("connect to cluster: %w", err))
		return
	}

	for si := range u.hop().Stages {
		st := u.hop().Stages[si]
		if st.Status == StatusSucceeded || st.Status == StatusSkipped {
			continue
		}
		now := e.now().UTC()
		if _, err := e.mutate(id, func(u *Upgrade) error {
			s := &u.hop().Stages[si]
			s.Status, s.StartedAt, s.FinishedAt, s.Message = StatusRunning, &now, nil, ""
			return nil
		}); err != nil {
			return
		}
		e.emit(id, Event{Hop: hopIdx, Stage: st.Name, Level: LevelState, Message: "Started " + string(st.Name)})

		timeout := StageTimeouts[st.Name]
		if t, ok := cluster.Executor().(StageTimeouter); ok {
			timeout = t.StageTimeout(st.Name)
		}
		sctx, cancel := context.WithTimeout(ctx, timeout)
		log := &stageLogger{e: e, id: id, hop: hopIdx, stage: st.Name}
		msg, skipped, err := e.execStage(sctx, u, st.Name, cluster, log)
		cancel()
		if err != nil {
			if errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				err = fmt.Errorf("%s did not finish within %s: %w", st.Name, timeout, err)
			}
			e.failStage(ctx, id, hopIdx, si, err)
			return
		}
		end := e.now().UTC()
		status := StatusSucceeded
		if skipped {
			status = StatusSkipped
		}
		if _, err := e.mutate(id, func(u *Upgrade) error {
			s := &u.hop().Stages[si]
			s.Status, s.FinishedAt, s.Message = status, &end, msg
			return nil
		}); err != nil {
			return
		}
		text := "Completed " + string(st.Name)
		if skipped {
			text = "Skipped " + string(st.Name)
		}
		if msg != "" {
			text += ": " + msg
		}
		e.emit(id, Event{Hop: hopIdx, Stage: st.Name, Level: LevelState, Message: text})
	}

	u, err = e.mutate(id, func(u *Upgrade) error {
		u.hop().Status = StatusSucceeded
		if u.CurrentHop == len(u.Hops) {
			e.finishLocked(u, StatusSucceeded, "")
			return nil
		}
		u.CurrentHop++
		u.hop().Status = StatusAwaitingApproval
		u.Status = StatusAwaitingApproval
		return nil
	})
	if err != nil {
		return
	}
	e.emit(id, Event{Hop: hopIdx, Level: LevelState, Message: fmt.Sprintf("Hop %d complete: cluster is on %s", hopIdx, u.Hops[hopIdx-1].To)})
	if u.Status == StatusSucceeded {
		e.emit(id, Event{Level: LevelState, Message: fmt.Sprintf("Upgrade complete: %s → %s", u.From, u.To)})
		return
	}
	e.emitApprovalNeeded(u)
}

func (e *Engine) execStage(ctx context.Context, u *Upgrade, name StageName, c Cluster, log Logger) (msg string, skipped bool, err error) {
	h := u.hop()
	switch name {
	case StagePreflight:
		return e.preflight(ctx, h, c, log)
	case StageControlPlane:
		return "", false, c.Executor().ControlPlane(ctx, h.To, log)
	case StageAddons:
		return "", false, c.Executor().Addons(ctx, h.To, log)
	case StageDataPlane:
		if h.DataPlane == provider.DataPlaneOptional {
			return "nodes stay on their current version for this hop (within the version-skew policy)", true, nil
		}
		return "", false, c.Executor().DataPlane(ctx, h.To, log)
	case StageVerify:
		return "", false, c.Verify(ctx, h.To, log)
	}
	return "", false, fmt.Errorf("unknown stage %q", name)
}

func (e *Engine) preflight(ctx context.Context, h *Hop, c Cluster, log Logger) (string, bool, error) {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return "", false, err
	}
	switch v {
	case h.To:
		log.Warn("Control plane is already on %s; resuming this hop", h.To)
		return "control plane already on " + h.To.String(), false, nil
	case h.From:
	default:
		return "", false, fmt.Errorf("cluster is on %s but this hop starts at %s; the cluster changed since planning, create a new plan", v, h.From)
	}

	log.Info("Re-inspecting the cluster for %s → %s", h.From, h.To)
	p, err := c.Plan(ctx, h.To)
	if err != nil {
		return "", false, err
	}
	for _, w := range p.CollectionWarnings {
		log.Warn("Incomplete data: %s", w)
	}
	var blockers []string
	for _, f := range p.Hops[0].Findings {
		switch f.Severity {
		case check.SeverityBlocker:
			blockers = append(blockers, f.Title+" ("+f.Resource+")")
		case check.SeverityWarning:
			log.Warn("%s", f.Title)
		}
	}
	if len(blockers) > 0 {
		if !h.overrideBlockers() {
			for _, b := range blockers {
				log.Warn("Blocker: %s", b)
			}
			return "", false, fmt.Errorf("%d blocker(s) must be resolved or explicitly overridden at approval: %s", len(blockers), strings.Join(blockers, "; "))
		}
		for _, b := range blockers {
			log.Warn("Blocker overridden by approver: %s", b)
		}
	}
	return fmt.Sprintf("%d blocker(s), %d warning(s)", len(blockers), p.Summary.Warnings), false, nil
}

func (e *Engine) failStage(ctx context.Context, id string, hopIdx, si int, cause error) {
	e.mu.Lock()
	by, cancelled := e.cancelBy[id]
	e.mu.Unlock()

	// Process shutdown: leave the upgrade running so Resume continues it.
	if ctx.Err() != nil && !cancelled {
		return
	}
	end := e.now().UTC()
	status := StatusFailed
	if cancelled {
		status = StatusCancelled
	}
	var stageName StageName
	_, _ = e.mutate(id, func(u *Upgrade) error {
		h := &u.Hops[hopIdx-1]
		s := &h.Stages[si]
		stageName = s.Name
		s.Status, s.FinishedAt, s.Message = status, &end, cause.Error()
		h.Status = status
		msg := cause.Error()
		if cancelled {
			msg = "cancelled by " + by
		}
		e.finishLocked(u, status, msg)
		return nil
	})
	if cancelled {
		e.emit(id, Event{Hop: hopIdx, Stage: stageName, Level: LevelState, Message: fmt.Sprintf("Upgrade cancelled by %s during %s. Cloud operations already submitted continue on the provider side; check the cluster before planning again.", by, stageName)})
		return
	}
	e.emit(id, Event{Hop: hopIdx, Stage: stageName, Level: LevelError, Message: fmt.Sprintf("%s failed: %v", stageName, cause)})
	e.emit(id, Event{Hop: hopIdx, Level: LevelState, Message: "Upgrade paused. Fix the cause, then retry the hop or cancel the upgrade."})
}

func (e *Engine) emitApprovalNeeded(u *Upgrade) {
	h := u.hop()
	msg := fmt.Sprintf("Awaiting approval for hop %d: %s → %s", h.Index, h.From, h.To)
	if h.Blockers > 0 {
		msg += fmt.Sprintf(" (plan reported %d blocker(s))", h.Blockers)
	}
	e.emit(u.ID, Event{Hop: h.Index, Level: LevelState, Message: msg})
}

func (e *Engine) emit(id string, ev Event) {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	if _, ok := e.seq[id]; !ok {
		var last int64
		if evs, err := e.store.Events(id, 0); err == nil && len(evs) > 0 {
			last = evs[len(evs)-1].Seq
		}
		e.seq[id] = last
	}
	e.seq[id]++
	ev.Seq = e.seq[id]
	ev.Time = e.now().UTC()
	_ = e.store.AppendEvent(id, ev)
	for ch := range e.subs[id] {
		select {
		case ch <- ev:
		default:
			delete(e.subs[id], ch)
			close(ch)
		}
	}
}

type stageLogger struct {
	e     *Engine
	id    string
	hop   int
	stage StageName
}

func (l *stageLogger) Info(format string, args ...any) {
	l.e.emit(l.id, Event{Hop: l.hop, Stage: l.stage, Level: LevelInfo, Message: fmt.Sprintf(format, args...)})
}

func (l *stageLogger) Warn(format string, args ...any) {
	l.e.emit(l.id, Event{Hop: l.hop, Stage: l.stage, Level: LevelWarn, Message: fmt.Sprintf(format, args...)})
}

func (l *stageLogger) Progress(done, total int, unit, format string, args ...any) {
	l.e.emit(l.id, Event{Hop: l.hop, Stage: l.stage, Level: LevelInfo, Message: fmt.Sprintf(format, args...),
		Progress: &Progress{Done: done, Total: total, Unit: unit}})
}
