package analyze

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/cost"
	"github.com/vertti/ci-snitch/internal/stats"
)

// TypeRunner is the finding type for runner sizing analysis.
const TypeRunner = "runner"

// RunnerDetail contains runner sizing analysis for a job.
type RunnerDetail struct {
	WorkflowName string   `json:"workflow_name"`
	JobName      string   `json:"job_name"`
	RunnerLabel  string   `json:"runner_label"`
	Cores        int      `json:"cores"`
	MedianDur    Duration `json:"median_duration"`
	Runs         int      `json:"runs"`
	Multiplier   float64  `json:"multiplier"`
	Issue        string   `json:"issue"` // "oversized" or "undersized"
	Suggestion   string   `json:"suggestion"`
}

// DetailType implements FindingDetail.
func (RunnerDetail) DetailType() string { return TypeRunner }

// RunnerAnalyzer flags jobs with mismatched runner sizes.
type RunnerAnalyzer struct{}

// Name implements Analyzer.
func (RunnerAnalyzer) Name() string { return TypeRunner }

const (
	minRunsForRunner = 5
	// A short job on a large runner is wasteful
	oversizedThresholdSec = 120 // 2 minutes
	oversizedMinCores     = 8
	// A long job on a small runner could benefit from more cores
	undersizedThresholdSec = 900 // 15 minutes
	undersizedMaxCores     = 4
)

// Analyze implements Analyzer.
func (RunnerAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	type jobKey struct {
		wfID  int64
		job   string
		label string
	}
	type jobAccum struct {
		durations []float64
		label     string
		cores     int
		mult      float64
	}

	jobs := make(map[jobKey]*jobAccum)
	for _, d := range ac.Details {
		for _, j := range d.Jobs {
			dur := j.Duration().Seconds()
			if dur <= 0 || len(j.Labels) == 0 {
				continue
			}
			label := strings.Join(j.Labels, ",")
			k := jobKey{d.Run.WorkflowID, j.Name, label}
			if jobs[k] == nil {
				cores := parseCorCount(label)
				jobs[k] = &jobAccum{
					label: label,
					cores: cores,
					mult:  cost.LookupMultiplier(j.Labels),
				}
			}
			jobs[k].durations = append(jobs[k].durations, dur)
		}
	}

	var findings []Finding
	for k, ja := range jobs {
		if len(ja.durations) < minRunsForRunner || ja.cores == 0 {
			continue
		}
		wfName := ac.WorkflowName(k.wfID)
		median := stats.Median(ja.durations)

		var issue, suggestion string
		switch {
		case ja.cores >= oversizedMinCores && median < oversizedThresholdSec:
			issue = "oversized"
			suggestion = fmt.Sprintf("job takes %s on %d cores — consider downsizing to save ~%.0fx cost",
				fmtSeconds(median), ja.cores, ja.mult)
		case ja.cores <= undersizedMaxCores && median > undersizedThresholdSec:
			issue = "undersized"
			suggestion = fmt.Sprintf("job takes %s on %d cores — consider larger runner to reduce wait",
				fmtSeconds(median), ja.cores)
		default:
			continue
		}

		findings = append(findings, Finding{
			Type:        TypeRunner,
			Severity:    SeverityInfo,
			Title:       fmt.Sprintf("Runner sizing: %s / %s", wfName, k.job),
			Description: suggestion,
			Detail: RunnerDetail{
				WorkflowName: wfName,
				JobName:      k.job,
				RunnerLabel:  ja.label,
				Cores:        ja.cores,
				MedianDur:    Duration(time.Duration(median * float64(time.Second))),
				Runs:         len(ja.durations),
				Multiplier:   ja.mult,
				Issue:        issue,
				Suggestion:   suggestion,
			},
		})
	}

	// Sort: oversized first (cost savings), then by core count descending
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(RunnerDetail)
		bd, _ := b.Detail.(RunnerDetail)
		if ad.Issue != bd.Issue {
			if ad.Issue == "oversized" {
				return -1
			}
			return 1
		}
		return bd.Cores - ad.Cores
	})

	return findings, nil
}

// parseCorCount extracts core count from runner labels like "blacksmith-16vcpu-ubuntu-2404".
func parseCorCount(label string) int {
	lower := strings.ToLower(label)
	// Pattern: NNvcpu or NN-cores
	for part := range strings.SplitSeq(lower, "-") {
		if strings.HasSuffix(part, "vcpu") {
			n := 0
			for _, c := range part {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				} else {
					break
				}
			}
			if n > 0 {
				return n
			}
		}
		if strings.HasSuffix(part, "cores") || strings.HasSuffix(part, "core") {
			n := 0
			for _, c := range part {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				} else {
					break
				}
			}
			if n > 0 {
				return n
			}
		}
	}
	// Standard GitHub runners
	switch {
	case strings.Contains(lower, "ubuntu") || strings.Contains(lower, "linux"):
		return 2 // default GitHub-hosted Linux
	case strings.Contains(lower, "windows"):
		return 2
	case strings.Contains(lower, "macos"):
		return 4 // M1 runners
	}
	return 0
}

func fmtSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second)).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	sec := int(d.Seconds()) % 60
	if sec == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, sec)
}
