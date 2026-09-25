// Package compat holds Jin's compatibility knowledge base: API removals and add-on metadata.
package compat

import (
	"embed"
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/jin-k8s/jin/internal/kube"
)

//go:embed data/*.yaml
var dataFS embed.FS

const supportedSchema = 1

type APIRemoval struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	RemovedIn   kube.Version `json:"removedIn"`
	Replacement string       `json:"replacement,omitempty"`
	Notes       string       `json:"notes,omitempty"`
}

type VersionPolicy string

const PolicyKubeletSkew VersionPolicy = "kubelet-skew"

type WorkloadMatch struct {
	Kind       string   `json:"kind"`
	Namespaces []string `json:"namespaces"`
	Names      []string `json:"names"`
	Container  string   `json:"container,omitempty"`
}

type Addon struct {
	Name          string          `json:"name"`
	EKSAddonName  string          `json:"eksAddonName,omitempty"`
	VersionPolicy VersionPolicy   `json:"versionPolicy,omitempty"`
	Workloads     []WorkloadMatch `json:"workloads"`
}

type KB struct {
	removals        map[string]APIRemoval
	addons          []Addon
	sources         []string
	verifiedThrough kube.Version
}

type removalsFile struct {
	SchemaVersion   int          `json:"schemaVersion"`
	VerifiedThrough kube.Version `json:"verifiedThrough"`
	Sources         []string     `json:"sources"`
	Removals        []struct {
		RemovedIn kube.Version `json:"removedIn"`
		APIs      []struct {
			APIVersion  string   `json:"apiVersion"`
			Kinds       []string `json:"kinds"`
			Replacement string   `json:"replacement"`
			Notes       string   `json:"notes"`
		} `json:"apis"`
	} `json:"removals"`
}

type addonsFile struct {
	SchemaVersion int     `json:"schemaVersion"`
	Addons        []Addon `json:"addons"`
}

// Load parses the embedded knowledge base.
func Load() (*KB, error) {
	var rf removalsFile
	if err := readYAML("data/api-removals.yaml", &rf); err != nil {
		return nil, err
	}
	var af addonsFile
	if err := readYAML("data/addons.yaml", &af); err != nil {
		return nil, err
	}
	if rf.SchemaVersion != supportedSchema || af.SchemaVersion != supportedSchema {
		return nil, fmt.Errorf("unsupported compatibility data schema (want %d)", supportedSchema)
	}

	if rf.VerifiedThrough.IsZero() {
		return nil, fmt.Errorf("api-removals.yaml: verifiedThrough is required")
	}
	kb := &KB{removals: map[string]APIRemoval{}, addons: af.Addons, sources: rf.Sources, verifiedThrough: rf.VerifiedThrough}
	for _, r := range rf.Removals {
		for _, a := range r.APIs {
			for _, k := range a.Kinds {
				key := removalKey(a.APIVersion, k)
				if _, dup := kb.removals[key]; dup {
					return nil, fmt.Errorf("duplicate API removal entry %s", key)
				}
				kb.removals[key] = APIRemoval{
					APIVersion:  a.APIVersion,
					Kind:        k,
					RemovedIn:   r.RemovedIn,
					Replacement: a.Replacement,
					Notes:       a.Notes,
				}
			}
		}
	}
	return kb, nil
}

func readYAML(name string, out any) error {
	b, err := dataFS.ReadFile(name)
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

func removalKey(apiVersion, kind string) string { return apiVersion + "/" + kind }

func (kb *KB) Removal(apiVersion, kind string) (APIRemoval, bool) {
	r, ok := kb.removals[removalKey(apiVersion, kind)]
	return r, ok
}

// Removals returns all entries ordered by release, apiVersion and kind.
func (kb *KB) Removals() []APIRemoval {
	out := make([]APIRemoval, 0, len(kb.removals))
	for _, r := range kb.removals {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].RemovedIn.Compare(out[j].RemovedIn); c != 0 {
			return c < 0
		}
		if out[i].APIVersion != out[j].APIVersion {
			return out[i].APIVersion < out[j].APIVersion
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// RemovedKinds is the set of kinds that have at least one removed API version.
func (kb *KB) RemovedKinds() map[string]bool {
	out := map[string]bool{}
	for _, r := range kb.removals {
		out[r.Kind] = true
	}
	return out
}

func (kb *KB) Addons() []Addon { return kb.addons }

func (kb *KB) Addon(name string) (Addon, bool) {
	for _, a := range kb.addons {
		if a.Name == name {
			return a, true
		}
	}
	return Addon{}, false
}

func (kb *KB) Sources() []string { return kb.sources }

// VerifiedThrough is the newest release whose API removals are covered by the knowledge base.
func (kb *KB) VerifiedThrough() kube.Version { return kb.verifiedThrough }
