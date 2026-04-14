package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vertti/ci-snitch/internal/cost"
	"github.com/vertti/ci-snitch/internal/stats"
)

// CostDetail contains cost estimation for a workflow.
type CostDetail struct {
	Workflow             string             `json:"workflow"`
	TotalRuns            int                `json:"total_runs"`
	BillableMinutes      float64            `json:"billable_minutes"`
	SelfHostedMinutes    float64            `json:"self_hosted_minutes"` // minutes on self-hosted runners (free)
	DailyRate            float64            `json:"daily_rate"`          // billable minutes per day
	PriorityScore        float64            `json:"priority_score"`      // higher = more optimization value
	DailySavingsEstimate float64            `json:"daily_savings_estimate"`
	Jobs                 []JobCostBreakdown `json:"jobs"`
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

	type jobKey struct {
		wfID int64
		job  string
	}
	type jobAccum struct {
		billable     float64
		selfHosted   float64
		multiplier   float64
		isSelfHosted bool
		runs         int
	}

	wfRuns := make(map[int64]int)
	wfBillable := make(map[int64][]float64) // per-run billable minutes (for priority scoring)
	jobCosts := make(map[jobKey]*jobAccum)
	var minTime, maxTime time.Time

	for _, d := range ac.Details {
		wfID := d.Run.WorkflowID
		wfRuns[wfID]++

		t := d.Run.CreatedAt
		if minTime.IsZero() || t.Before(minTime) {
			minTime = t
		}
		if t.After(maxTime) {
			maxTime = t
		}

		var runBillable float64
		for _, j := range d.Jobs {
			k := jobKey{wfID, j.Name}
			if jobCosts[k] == nil {
				sh := cost.IsSelfHosted(j.Labels)
				jobCosts[k] = &jobAccum{
					multiplier:   cost.LookupMultiplier(j.Labels),
					isSelfHosted: sh,
				}
			}
			jc := jobCosts[k]
			rawMinutes := cost.BillableMinutes(j.Duration())
			if jc.isSelfHosted {
				jc.selfHosted += rawMinutes
			} else {
				billable := rawMinutes * jc.multiplier
				jc.billable += billable
				runBillable += billable
			}
			jc.runs++
		}
		if runBillable > 0 {
			wfBillable[wfID] = append(wfBillable[wfID], runBillable)
		}
	}

	days := maxTime.Sub(minTime).Hours() / 24
	if days < 1 {
		days = 1
	}

	var findings []Finding
	for wfID, runs := range wfRuns {
		wfName := ac.WorkflowName(wfID)
		var totalBillable, totalSelfHosted float64
		var jobs []JobCostBreakdown

		for k, jc := range jobCosts {
			if k.wfID != wfID {
				continue
			}
			totalBillable += jc.billable
			totalSelfHosted += jc.selfHosted
			jobs = append(jobs, JobCostBreakdown{
				Name:            k.job,
				BillableMinutes: jc.billable + jc.selfHosted,
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

		// Priority score: daily rate × improvement potential (p95/median ratio).
		// Uses per-run billable minutes for consistent units.
		var priorityScore, dailySavings float64
		if billable := wfBillable[wfID]; len(billable) >= 5 {
			median := stats.Median(billable)
			p95 := stats.Percentile(billable, 95)
			p25 := stats.Percentile(billable, 25)
			if median > 0 {
				improvementPotential := p95 / median
				priorityScore = (totalBillable / days) * improvementPotential
				// Estimated daily savings in billable minutes if median were brought to p25
				runsPerDay := float64(runs) / days
				dailySavings = (median - p25) * runsPerDay
			}
		}

		findings = append(findings, Finding{
			Type:     "cost",
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("Workflow %q cost estimate", wfName),
			Description: fmt.Sprintf("%.0f billable minutes (%.0f/day) across %d runs",
				totalBillable, totalBillable/days, runs),
			Detail: CostDetail{
				Workflow:             wfName,
				TotalRuns:            runs,
				BillableMinutes:      totalBillable,
				SelfHostedMinutes:    totalSelfHosted,
				DailyRate:            totalBillable / days,
				PriorityScore:        priorityScore,
				DailySavingsEstimate: dailySavings,
				Jobs:                 jobs,
			},
		})
	}

	// Sort by priority score descending (higher = more optimization value)
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(CostDetail)
		bd, _ := b.Detail.(CostDetail)
		if bd.PriorityScore > ad.PriorityScore {
			return 1
		}
		if bd.PriorityScore < ad.PriorityScore {
			return -1
		}
		return 0
	})

	return findings, nil
}
