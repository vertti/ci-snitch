// Package cost provides runner cost estimation for GitHub Actions.
package cost

import (
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultMultipliers maps runner labels to GitHub's published billing multipliers.
// GitHub bills per-minute with different rates per runner OS/size.
// See: https://docs.github.com/en/billing/managing-billing-for-your-products/managing-billing-for-github-actions/about-billing-for-github-actions
var defaultMultipliers = map[string]float64{
	"ubuntu-latest":  1,
	"ubuntu-24.04":   1,
	"ubuntu-22.04":   1,
	"ubuntu-20.04":   1,
	"windows-latest": 2,
	"windows-2025":   2,
	"windows-2022":   2,
	"windows-2019":   2,
	"macos-latest":   10,
	"macos-15":       10,
	"macos-14":       10,
	"macos-13":       10,
}

// Model holds runner cost configuration. Use DefaultModel() for standard GitHub rates.
type Model struct {
	multipliers map[string]float64
}

// DefaultModel returns a Model with GitHub's published billing multipliers.
func DefaultModel() Model {
	m := make(map[string]float64, len(defaultMultipliers))
	maps.Copy(m, defaultMultipliers)
	return Model{multipliers: m}
}

// largerRunnerRe matches GitHub larger runner labels like "ubuntu-latest-16-cores".
// Per GitHub docs, the multiplier scales linearly with core count (1x per 2 cores for Linux).
var largerRunnerRe = regexp.MustCompile(`-(\d+)-cores?$`)

// IsSelfHosted reports whether the labels indicate a self-hosted runner.
func IsSelfHosted(labels []string) bool {
	return DefaultModel().IsSelfHosted(labels)
}

// IsSelfHosted reports whether the labels indicate a self-hosted runner.
func (Model) IsSelfHosted(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, "self-hosted") {
			return true
		}
	}
	return false
}

// LookupMultiplier returns the billing multiplier for a set of runner labels.
// Uses the default model.
func LookupMultiplier(labels []string) float64 {
	return DefaultModel().LookupMultiplier(labels)
}

// LookupMultiplier returns the billing multiplier for a set of runner labels.
// Self-hosted runners return 0 (free on GitHub billing).
// Checks each label against the known multiplier table, then tries
// larger-runner pattern matching. Returns 1.0 (Linux default) if no match.
func (m Model) LookupMultiplier(labels []string) float64 {
	if m.IsSelfHosted(labels) {
		return 0
	}
	for _, label := range labels {
		label = strings.ToLower(label)
		if mult, ok := m.multipliers[label]; ok {
			return mult
		}
		if mult, ok := largerRunnerMultiplier(label); ok {
			return mult
		}
	}
	return 1
}

// largerRunnerMultiplier extracts the core count from a larger runner label
// and returns the GitHub billing multiplier. Linux: cores/2, Windows: cores,
// macOS: cores*5 (matching GitHub's published rates).
func largerRunnerMultiplier(label string) (float64, bool) {
	matches := largerRunnerRe.FindStringSubmatch(label)
	if matches == nil {
		return 0, false
	}
	cores, err := strconv.Atoi(matches[1])
	if err != nil || cores < 2 {
		return 0, false
	}
	switch {
	case strings.HasPrefix(label, "windows"):
		return float64(cores), true
	case strings.HasPrefix(label, "macos"):
		return float64(cores) * 5, true
	default: // linux/ubuntu
		return float64(cores) / 2, true
	}
}

// BillableMinutes returns the billable minutes for a job duration.
// Uses the default model.
func BillableMinutes(d time.Duration) float64 {
	return DefaultModel().BillableMinutes(d)
}

// BillableMinutes returns the billable minutes for a job duration.
// GitHub rounds each job's duration up to the nearest whole minute.
func (Model) BillableMinutes(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return math.Ceil(d.Minutes())
}
