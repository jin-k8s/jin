package support

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/jin-k8s/jin/internal/kube"
)

type fakeEKS struct {
	pages [][]types.ClusterVersionInformation
}

func (f *fakeEKS) DescribeClusterVersions(_ context.Context, in *eks.DescribeClusterVersionsInput, _ ...func(*eks.Options)) (*eks.DescribeClusterVersionsOutput, error) {
	i := 0
	if in.NextToken != nil {
		i = 1
	}
	out := &eks.DescribeClusterVersionsOutput{ClusterVersions: f.pages[i]}
	if i == 0 && len(f.pages) > 1 {
		out.NextToken = aws.String("p2")
	}
	return out, nil
}

func date(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

func TestEKSCalendarAndAssess(t *testing.T) {
	f := &fakeEKS{pages: [][]types.ClusterVersionInformation{
		{{ClusterVersion: aws.String("1.31"), Status: types.ClusterVersionStatusStandardSupport, EndOfStandardSupportDate: date("2026-11-26"), EndOfExtendedSupportDate: date("2027-11-26")}},
		{{ClusterVersion: aws.String("1.29"), Status: types.ClusterVersionStatusExtendedSupport, EndOfStandardSupportDate: date("2025-03-23"), EndOfExtendedSupportDate: date("2026-03-23")},
			{ClusterVersion: aws.String("1.30"), Status: "EXTENDED_SUPPORT", VersionStatus: types.VersionStatusExtendedSupport, EndOfStandardSupportDate: date("2025-07-23"), EndOfExtendedSupportDate: date("2026-12-01")}},
	}}
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	cal, err := EKSCalendar(context.Background(), f, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Versions) != 3 || cal.Versions[0].Version != kube.MustParseVersion("1.29") {
		t.Fatalf("calendar must be paginated and sorted: %+v", cal.Versions)
	}

	a := Assess(cal, kube.MustParseVersion("1.30"), kube.MustParseVersion("1.31"), now)
	if a.CurrentStatus != StatusExtended || a.CurrentSurchargePerYearUSD != 4380 || a.ExtendedSupportCostPerYearUSD != 4380 {
		t.Fatalf("extended cost: %+v", a)
	}
	if a.TargetEndOfStandard == nil || a.TargetEndOfStandard.Format("2006-01-02") != "2026-11-26" {
		t.Fatalf("target window: %+v", a.TargetEndOfStandard)
	}

	c := Assess(cal, kube.MustParseVersion("1.29"), kube.MustParseVersion("1.30"), now)
	if c.TargetStatus != StatusExtended || c.FirstStandard == nil || *c.FirstStandard != kube.MustParseVersion("1.31") {
		t.Fatalf("target in extended support must name the first standard version: %+v", c)
	}
	if a.FirstStandard != nil {
		t.Fatalf("standard target needs no first-standard hint: %v", a.FirstStandard)
	}

	b := Assess(cal, kube.MustParseVersion("1.31"), kube.MustParseVersion("1.32"), now)
	if b.CurrentSurchargePerYearUSD != 0 || b.DaysToEndOfStandard == nil || *b.DaysToEndOfStandard != 63 {
		t.Fatalf("standard window: %+v days=%v", b, b.DaysToEndOfStandard)
	}
	if Assess(nil, kube.Version{}, kube.Version{}, now) != nil {
		t.Fatal("nil calendar must yield nil assessment")
	}
}
