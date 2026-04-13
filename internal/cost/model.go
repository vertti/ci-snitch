// Package cost provides runner cost estimation for GitHub Actions.
package cost

import (
	"math"
	"strings"
	"time"
)

// Multiplier maps runner labels to GitHub's published billing multipliers.
// GitHub bills per-minute with different rates per runner OS/size.
// See: https://docs.github.com/en/billing/managing-billing-for-your-products/managing-billing-for-github-actions/about-billing-for-github-actions
var Multiplier = map[string]float64{
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

// LookupMultiplier returns the billing multiplier for a set of runner labels.
// Checks each label against the known multiplier table.
// Returns 1.0 (Linux default) if no label matches.
func LookupMultiplier(labels []string) float64 {
	for _, label := range labels {
		label = strings.ToLower(label)
		if m, ok := Multiplier[label]; ok {
			return m
		}
	}
	return 1
}

// BillableMinutes returns the billable minutes for a job duration.
// GitHub rounds each job's duration up to the nearest whole minute.
func BillableMinutes(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return math.Ceil(d.Minutes())
}
