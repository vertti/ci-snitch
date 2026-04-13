package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vertti/ci-snitch/internal/cost"
)

// CostDetail contains cost estimation for a workflow.
type CostDetail struct {
	Workflow        string             `json:"workflow"`
	TotalRuns       int                `json:"total_runs"`
	BillableMinutes float64            `json:"billable_minutes"`
	DailyRate       float64            `json:"daily_rate"` // billable minutes per day
	Jobs            []JobCostBreakdown `json:"jobs"`
}

// JobCostBreakdown holds cost info for a single job within a workflow.
type JobCostBreakdown struct {
	Name            string  `json:"name"`
	BillableMinutes float64 `json:"billable_minutes"`
	Multiplier      float64 `json:"multiplier"`
	Runs            int     `json:"runs"`
}

// DetailType implements FindingDetail.
func (CostDetail) DetailType() string { return "cost" }

// CostAnalyzer estimates CI cost per workflow based on job durations and runner types.
type CostAnalyzer struct{}

// Name implements Analyzer.
func (CostAnalyzer) Name() string { return "cost" }

// Analyze implements Analyzer.
func (CostAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	type jobKey struct{ wf, job string }
	type jobAccum struct {
		billable   float64
		multiplier float64
		runs       int
	}

	wfRuns := make(map[string]int)
	jobCosts := make(map[jobKey]*jobAccum)
	var minTime, maxTime time.Time

	for _, d := range ac.Details {
		name := d.Run.WorkflowName
		wfRuns[name]++

		t := d.Run.CreatedAt
		if minTime.IsZero() || t.Before(minTime) {
			minTime = t
		}
		if t.After(maxTime) {
			maxTime = t
		}

		for _, j := range d.Jobs {
			k := jobKey{name, j.Name}
			if jobCosts[k] == nil {
				jobCosts[k] = &jobAccum{
					multiplier: cost.LookupMultiplier(j.Labels),
				}
			}
			jc := jobCosts[k]
			jc.billable += cost.BillableMinutes(j.Duration()) * jc.multiplier
			jc.runs++
		}
	}

	days := maxTime.Sub(minTime).Hours() / 24
	if days < 1 {
		days = 1
	}

	var findings []Finding
	for wf, runs := range wfRuns {
		var totalBillable float64
		var jobs []JobCostBreakdown

		for k, jc := range jobCosts {
			if k.wf != wf {
				continue
			}
			totalBillable += jc.billable
			jobs = append(jobs, JobCostBreakdown{
				Name:            k.job,
				BillableMinutes: jc.billable,
				Multiplier:      jc.multiplier,
				Runs:            jc.runs,
			})
		}

		// Sort jobs by billable minutes descending
		slices.SortFunc(jobs, func(a, b JobCostBreakdown) int {
			if b.BillableMinutes > a.BillableMinutes {
				return 1
			}
			if b.BillableMinutes < a.BillableMinutes {
				return -1
			}
			return 0
		})

		findings = append(findings, Finding{
			Type:     "cost",
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("Workflow %q cost estimate", wf),
			Description: fmt.Sprintf("%.0f billable minutes (%.0f/day) across %d runs",
				totalBillable, totalBillable/days, runs),
			Detail: CostDetail{
				Workflow:        wf,
				TotalRuns:       runs,
				BillableMinutes: totalBillable,
				DailyRate:       totalBillable / days,
				Jobs:            jobs,
			},
		})
	}

	// Sort by billable minutes descending
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(CostDetail)
		bd, _ := b.Detail.(CostDetail)
		if bd.BillableMinutes > ad.BillableMinutes {
			return 1
		}
		if bd.BillableMinutes < ad.BillableMinutes {
			return -1
		}
		return 0
	})

	return findings, nil
}
