package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionAndHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "jin ") {
		t.Fatalf("version: code %d out %q err %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"plan", "--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "--fail-on-blockers") {
		t.Fatalf("plan help: code %d", code)
	}
}

func TestPlanRejectsBadInputBeforeContactingCluster(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "--target", "latest"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "invalid Kubernetes version") {
		t.Fatalf("code %d err %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"plan", "-o", "yaml"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "unknown output format") {
		t.Fatalf("code %d err %q", code, errOut.String())
	}
}

func TestRunsListEmpty(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"runs", "list", "--runs-dir", t.TempDir()}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "ID") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errOut.String())
	}
}
