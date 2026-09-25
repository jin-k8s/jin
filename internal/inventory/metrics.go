package inventory

import (
	"bufio"
	"bytes"
	"regexp"
	"sort"
	"strconv"

	"github.com/jin-k8s/jin/internal/kube"
)

var (
	deprecatedMetricRe = regexp.MustCompile(`^apiserver_requested_deprecated_apis\{([^}]*)\}\s+(\S+)`)
	labelPairRe        = regexp.MustCompile(`(\w+)="((?:[^"\\]|\\.)*)"`)
)

// parseDeprecatedAPIMetrics reads apiserver_requested_deprecated_apis from Prometheus text output.
func parseDeprecatedAPIMetrics(raw []byte) []DeprecatedAPIRequest {
	seen := map[DeprecatedAPIRequest]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		m := deprecatedMetricRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		if v, err := strconv.ParseFloat(m[2], 64); err != nil || v == 0 {
			continue
		}
		labels := map[string]string{}
		for _, p := range labelPairRe.FindAllStringSubmatch(m[1], -1) {
			labels[p[1]] = p[2]
		}
		req := DeprecatedAPIRequest{
			Group:       labels["group"],
			Version:     labels["version"],
			Resource:    labels["resource"],
			Subresource: labels["subresource"],
		}
		if rr := labels["removed_release"]; rr != "" {
			if v, err := kube.ParseVersion(rr); err == nil {
				req.RemovedRelease = v
			}
		}
		seen[req] = true
	}

	out := make([]DeprecatedAPIRequest, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.APIVersion() != b.APIVersion() {
			return a.APIVersion() < b.APIVersion()
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Subresource < b.Subresource
	})
	return out
}
