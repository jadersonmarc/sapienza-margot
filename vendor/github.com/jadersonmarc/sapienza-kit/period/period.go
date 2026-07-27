// Package period defines the billing/usage period convention shared across the
// platform: a calendar month in America/Sao_Paulo (BRT). The usage counters and
// invoice close reset at midnight in Brasília on the 1st — not in UTC. Every
// producer of UsageRecorded and every reader of usage_counters.period must use
// this so a count near the month boundary never splits across two periods.
//
// Mirrors sapienza-core/lib/billing/period.ts (currentPeriod).
package period

import "time"

// loc is America/Sao_Paulo; falls back to a fixed UTC-3 zone if the tzdata is
// unavailable (Brazil has had no DST since 2019, so UTC-3 is correct today).
var loc = mustLoc()

func mustLoc() *time.Location {
	l, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.FixedZone("BRT", -3*60*60)
	}
	return l
}

// Current returns the current period "YYYY-MM" in São Paulo time.
func Current(now time.Time) string {
	return now.In(loc).Format("2006-01")
}
