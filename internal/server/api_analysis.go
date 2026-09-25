package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jin-k8s/jin/internal/migrate"
	"github.com/jin-k8s/jin/internal/policy"
	"github.com/jin-k8s/jin/internal/snapshot"
)

func listLimit(n int64) metav1.ListOptions { return metav1.ListOptions{Limit: n} }

func policyDefault() policy.Policy { return policy.Default }

func (s *Server) snapshotOf(ctx context.Context, name string) (*snapshot.Snapshot, error) {
	c, err := s.cfg.Env.Connect(name)
	if err != nil {
		return nil, err
	}
	return s.cfg.Env.Snapshot(ctx, c)
}

// compare checks a replacement (green) cluster against the live (blue) one before cutover.
func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Target string `json:"target"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Source == "" || req.Target == "" || req.Source == req.Target {
		writeError(w, http.StatusBadRequest, "choose two different clusters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	type res struct {
		s   *snapshot.Snapshot
		err error
	}
	src, dst := make(chan res, 1), make(chan res, 1)
	go func() { s1, err := s.snapshotOf(ctx, req.Source); src <- res{s1, err} }()
	go func() { s2, err := s.snapshotOf(ctx, req.Target); dst <- res{s2, err} }()
	a, b := <-src, <-dst
	if err := errors.Join(a.err, b.err); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	p := snapshot.Compare(a.s, b.s)
	s.record(r, "compare.run", req.Source+" → "+req.Target, fmt.Sprintf("ready=%v missing=%d notReady=%d", p.Ready, p.Summary.Missing, p.Summary.NotReady))
	writeJSON(w, http.StatusOK, p)
}

// assess reports the cloud bindings that must change to run the cluster on another provider.
func (s *Server) assess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context string `json:"context"`
		Target  string `json:"target"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	snap, err := s.snapshotOf(ctx, req.Context)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a, err := migrate.Assess(snap, req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "assess.run", req.Context, fmt.Sprintf("%s → %s size %s", a.Source, a.Target, a.Size))
	writeJSON(w, http.StatusOK, a)
}
