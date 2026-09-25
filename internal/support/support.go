// Package support models Kubernetes version support windows and what falling behind costs.
package support

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/jin-k8s/jin/internal/kube"
)

const (
	StatusStandard    = "standard-support"
	StatusExtended    = "extended-support"
	StatusUnsupported = "unsupported"
)

// EKS list prices per cluster-hour. Negotiated discounts and regional differences are not modelled.
const (
	EKSStandardHourlyUSD = 0.10
	EKSExtendedHourlyUSD = 0.60
	PricingSource        = "Amazon EKS list price per cluster-hour (standard $0.10, extended $0.60)"
	hoursPerYear         = 24 * 365
)

type Version struct {
	Version       kube.Version `json:"version"`
	Status        string       `json:"status"`
	ReleaseDate   *time.Time   `json:"releaseDate,omitempty"`
	EndOfStandard *time.Time   `json:"endOfStandardSupport,omitempty"`
	EndOfExtended *time.Time   `json:"endOfExtendedSupport,omitempty"`
}

type Calendar struct {
	Provider  string    `json:"provider"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetchedAt"`
	Versions  []Version `json:"versions"`
}

func (c *Calendar) Lookup(v kube.Version) (Version, bool) {
	for _, x := range c.Versions {
		if x.Version == v {
			return x, true
		}
	}
	return Version{}, false
}

// Assessment is what a plan reports about support windows and cost.
type Assessment struct {
	Provider      string       `json:"provider"`
	Current       kube.Version `json:"current"`
	CurrentStatus string       `json:"currentStatus"`
	EndOfStandard *time.Time   `json:"endOfStandardSupport,omitempty"`
	EndOfExtended *time.Time   `json:"endOfExtendedSupport,omitempty"`
	// DaysToEndOfStandard is negative once standard support has ended.
	DaysToEndOfStandard *int `json:"daysToEndOfStandardSupport,omitempty"`
	// ExtendedSupportCostPerYearUSD is the surcharge over standard support for one cluster.
	ExtendedSupportCostPerYearUSD float64 `json:"extendedSupportCostPerYearUsd"`
	// CurrentSurchargePerYearUSD is non-zero when the cluster is paying for extended support today.
	CurrentSurchargePerYearUSD float64      `json:"currentSurchargePerYearUsd"`
	Target                     kube.Version `json:"target"`
	TargetEndOfStandard        *time.Time   `json:"targetEndOfStandardSupport,omitempty"`
	PricingSource              string       `json:"pricingSource"`
	Source                     string       `json:"source"`
}

func Assess(cal *Calendar, current, target kube.Version, now time.Time) *Assessment {
	if cal == nil {
		return nil
	}
	a := &Assessment{Provider: cal.Provider, Current: current, Target: target, Source: cal.Source}
	if cal.Provider == "eks" {
		a.PricingSource = PricingSource
		a.ExtendedSupportCostPerYearUSD = math.Round((EKSExtendedHourlyUSD - EKSStandardHourlyUSD) * hoursPerYear)
	}
	if v, ok := cal.Lookup(current); ok {
		a.CurrentStatus = v.Status
		a.EndOfStandard, a.EndOfExtended = v.EndOfStandard, v.EndOfExtended
		if v.EndOfStandard != nil {
			d := int(math.Floor(v.EndOfStandard.Sub(now).Hours() / 24))
			a.DaysToEndOfStandard = &d
		}
		if v.Status == StatusExtended {
			a.CurrentSurchargePerYearUSD = a.ExtendedSupportCostPerYearUSD
		}
	}
	if v, ok := cal.Lookup(target); ok {
		a.TargetEndOfStandard = v.EndOfStandard
	}
	return a
}

// EKSAPI is the subset of the EKS client used to read the support calendar.
type EKSAPI interface {
	DescribeClusterVersions(context.Context, *eks.DescribeClusterVersionsInput, ...func(*eks.Options)) (*eks.DescribeClusterVersionsOutput, error)
}

func EKSCalendar(ctx context.Context, api EKSAPI, now time.Time) (*Calendar, error) {
	cal := &Calendar{Provider: "eks", Source: "Amazon EKS DescribeClusterVersions API", FetchedAt: now.UTC()}
	in := &eks.DescribeClusterVersionsInput{IncludeAll: aws.Bool(true)}
	for {
		out, err := api.DescribeClusterVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describe EKS cluster versions: %w", err)
		}
		for _, v := range out.ClusterVersions {
			kv, err := kube.ParseVersion(aws.ToString(v.ClusterVersion))
			if err != nil {
				continue
			}
			cal.Versions = append(cal.Versions, Version{
				Version: kv, Status: string(v.Status), ReleaseDate: v.ReleaseDate,
				EndOfStandard: v.EndOfStandardSupportDate, EndOfExtended: v.EndOfExtendedSupportDate,
			})
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	sort.Slice(cal.Versions, func(i, j int) bool { return cal.Versions[i].Version.Less(cal.Versions[j].Version) })
	return cal, nil
}
