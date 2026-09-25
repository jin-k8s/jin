package inventory

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	helmSecretType  = "helm.sh/release.v1" //nolint:gosec // Secret type name, not a credential.
	maxReleaseBytes = 64 << 20
)

var (
	gzipMagic = []byte{0x1f, 0x8b, 0x08}
	docSep    = regexp.MustCompile(`(?m)^---\s*$`)
)

type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Manifest  string `json:"manifest"`
	Chart     struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

type manifestHeader struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// decodeHelmRelease decodes Helm v3 secret storage: base64 text of (usually gzipped) release JSON.
func decodeHelmRelease(data []byte) (*helmRelease, error) {
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(raw, data)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	raw = raw[:n]

	if bytes.HasPrefix(raw, gzipMagic) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		raw, err = io.ReadAll(io.LimitReader(zr, maxReleaseBytes+1))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		if len(raw) > maxReleaseBytes {
			return nil, errors.New("release payload exceeds size limit")
		}
	}

	var rel helmRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &rel, nil
}

// parseManifest extracts object headers from a multi-document YAML manifest.
func parseManifest(manifest string) ([]manifestHeader, error) {
	var out []manifestHeader
	var errs []error
	for _, doc := range docSep.Split(manifest, -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var h manifestHeader
		if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
			errs = append(errs, err)
			continue
		}
		if h.APIVersion == "" || h.Kind == "" {
			continue
		}
		out = append(out, h)
	}
	return out, errors.Join(errs...)
}
