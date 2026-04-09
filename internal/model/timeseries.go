package model

import "time"

// TimePoint is a single observation in a time series.
type TimePoint struct {
	Time  time.Time
	Value float64
	Label string // e.g. commit SHA or run ID for context
}

// TimeSeries is a named sequence of time-value observations.
type TimeSeries struct {
	Name   string
	Points []TimePoint
}
