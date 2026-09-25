package kube

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version is a Kubernetes major.minor version. Patch levels are irrelevant to upgrade planning.
type Version struct {
	Major int
	Minor int
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// ParseVersion accepts forms such as "1.30", "v1.30.4" and "v1.30.4-eks-a737599".
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("invalid Kubernetes version %q", s)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid Kubernetes version %q: %w", s, err)
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, fmt.Errorf("invalid Kubernetes version %q: %w", s, err)
	}
	return Version{Major: major, Minor: minor}, nil
}

func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string {
	if v.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func (v Version) IsZero() bool { return v.Major == 0 && v.Minor == 0 }

func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	default:
		return cmpInt(v.Minor, o.Minor)
	}
}

func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

func (v Version) Next() Version { return Version{Major: v.Major, Minor: v.Minor + 1} }

// MinorsBehind returns how many minor versions v lags o; negative when v is newer.
func (v Version) MinorsBehind(o Version) int { return o.Minor - v.Minor }

func (v Version) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

func (v *Version) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*v = Version{}
		return nil
	}
	p, err := ParseVersion(string(b))
	if err != nil {
		return err
	}
	*v = p
	return nil
}

// MaxKubeletSkew is how many minors a kubelet or kube-proxy may lag kube-apiserver.
// Since 1.28 the policy allows n-3; before that n-2.
func MaxKubeletSkew(apiserver Version) int {
	if apiserver.Compare(Version{Major: 1, Minor: 28}) >= 0 {
		return 3
	}
	return 2
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
