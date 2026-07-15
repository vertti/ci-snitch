package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vertti/ci-snitch/internal/cost"
	"github.com/vertti/ci-snitch/internal/model"
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
	Name              string  `json:"name"`
	BillableMinutes   float64 `json:"billable_minutes"`
	SelfHostedMinutes float64 `json:"self_hosted_minutes,omitempty"`
	Multiplier        float64 `json:"multiplier"`
	Runs              int     `json:"runs"`
}

// DetailType implements FindingDetail.
func (CostDetail) DetailType() string { return TypeCost }

// CostAnalyzer estimates CI cost per workflow based on job durations and runner types.
type CostAnalyzer struct{}

// Name implements Analyzer.
func (CostAnalyzer) Name() string { return "cost" }

type costJobKey struct {
	wfID int64
	job  string
}

type jobAccum struct {
	billable   float64
	selfHosted float64
	multiplier float64   // most recent run's multiplier (display only)
	lastSeen   time.Time // CreatedAt of the run that set multiplier
	runs       int
}

type costAccum struct {
	wfRuns     map[int64]int
	wfBillable map[int64][]float64 // per-run billable minutes (for priority scoring)
	jobCosts   map[costJobKey]*jobAccum
	minTime    time.Time
	maxTime    time.Time
}

func accumulateCosts(details []model.RunDetail) costAccum {
	acc := costAccum{
		wfRuns:     make(map[int64]int),
		wfBillable: make(map[int64][]float64),
		jobCosts:   make(map[costJobKey]*jobAccum),
	}

	for i := range details {
		wfID := details[i].Run.WorkflowID
		acc.wfRuns[wfID]++

		t := details[i].Run.CreatedAt
		if acc.minTime.IsZero() || t.Before(acc.minTime) {
			acc.minTime = t
		}
		if t.After(acc.maxTime) {
			acc.maxTime = t
		}

		var runBillable float64
		for j := range details[i].Jobs {
			k := costJobKey{wfID, details[i].Jobs[j].Name}
			if acc.jobCosts[k] == nil {
				acc.jobCosts[k] = &jobAccum{}
			}
			jc := acc.jobCosts[k]

			// Price each run by its own labels: a job migrated mid-window
			// (e.g. ubuntu-latest -> self-hosted) must not have every run
			// billed at whichever variant happened to be seen first.
			labels := details[i].Jobs[j].Labels
			multiplier := cost.LookupMultiplier(labels)
			if jc.lastSeen.IsZero() || !t.Before(jc.lastSeen) {
				jc.multiplier = multiplier
				jc.lastSeen = t
			}

			rawMinutes := cost.BillableMinutes(details[i].Jobs[j].Duration())
			if cost.IsSelfHosted(labels) {
				jc.selfHosted += rawMinutes
			} else {
				billable := rawMinutes * multiplier
				jc.billable += billable
				runBillable += billable
			}
			jc.runs++
		}
		if runBillable > 0 {
			acc.wfBillable[wfID] = append(acc.wfBillable[wfID], runBillable)
		}
	}

	return acc
}

// Analyze implements Analyzer.
func (CostAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	// GitHub bills failed and cancelled runs too, so cost comes from the
	// unfiltered set. Fall back to Details when AllDetails isn't provided.
	details := ac.AllDetails
	if len(details) == 0 {
		details = ac.Details
	}
	if len(details) == 0 {
		return nil, nil
	}

	acc := accumulateCosts(details)
	wfRuns, wfBillable, jobCosts := acc.wfRuns, acc.wfBillable, acc.jobCosts

	days := acc.maxTime.Sub(acc.minTime).Hours() / 24
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
				Name:              k.job,
				BillableMinutes:   jc.billable,
				SelfHostedMinutes: jc.selfHosted,
				Multiplier:        jc.multiplier,
				Runs:              jc.runs,
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
