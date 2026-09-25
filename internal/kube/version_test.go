package kube

import (
	"encoding/json"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := map[string]Version{
		"1.30":                       {1, 30},
		"v1.30.4":                    {1, 30},
		"v1.30.4-eks-a737599":        {1, 30},
		"v1.29.8-gke.1031000":        {1, 29},
		"v1.31.0-minimal-eksbuild.3": {1, 31},
	}
	for in, want := range cases {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "latest", "1", "x1.2"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) succeeded, want error", bad)
		}
	}
}

func TestCompareAndSkew(t *testing.T) {
	a, b := MustParseVersion("1.29"), MustParseVersion("1.31")
	if !a.Less(b) || b.Less(a) || a.Compare(a) != 0 {
		t.Fatal("ordering broken")
	}
	if got := a.MinorsBehind(b); got != 2 {
		t.Fatalf("MinorsBehind = %d, want 2", got)
	}
	if MaxKubeletSkew(MustParseVersion("1.27")) != 2 || MaxKubeletSkew(MustParseVersion("1.28")) != 3 {
		t.Fatal("skew policy wrong")
	}
}

func TestVersionJSON(t *testing.T) {
	var s struct {
		V Version `json:"v"`
	}
	if err := json.Unmarshal([]byte(`{"v":"1.33"}`), &s); err != nil || s.V != (Version{1, 33}) {
		t.Fatalf("unmarshal: %v %v", s.V, err)
	}
	b, _ := json.Marshal(s)
	if string(b) != `{"v":"1.33"}` {
		t.Fatalf("marshal: %s", b)
	}
}
