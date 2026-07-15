// Package automation evaluates per-tenant inbound rules (off-hours, welcome,
// keyword) against an incoming message, returning the action to take.
package automation

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/jadersonmarc/sapienza-margot/internal/store"
)

// Rule types.
const (
	TypeOffHours = "off_hours"
	TypeWelcome  = "welcome"
	TypeKeyword  = "keyword"
)

// Rule is a parsed, enabled automation.
type Rule struct {
	Type     string
	Reply    string
	Handoff  bool
	Keywords []string  // keyword rules
	Schedule *Schedule // off_hours rules
}

// Schedule describes a tenant's business hours window.
type Schedule struct {
	Timezone string `json:"timezone"`
	Weekdays []int  `json:"weekdays"` // 0=Sunday .. 6=Saturday
	Start    string `json:"start"`    // "08:00"
	End      string `json:"end"`      // "18:00"
}

// Input is the inbound context an automation is evaluated against.
type Input struct {
	Text         string
	FirstMessage bool
	Now          time.Time
}

// Decision is the outcome of evaluating the rules.
type Decision struct {
	Triggered bool
	Reply     string
	Handoff   bool
}

type triggerConfig struct {
	Keywords []string `json:"keywords"`
	Timezone string   `json:"timezone"`
	Weekdays []int    `json:"weekdays"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
}

type actionConfig struct {
	Reply   string `json:"reply"`
	Handoff bool   `json:"handoff"`
}

// RulesFrom parses enabled store automations into evaluable rules, preserving order.
func RulesFrom(rows []store.Automation) ([]Rule, error) {
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		var trig triggerConfig
		if len(row.Trigger) > 0 {
			if err := json.Unmarshal(row.Trigger, &trig); err != nil {
				return nil, err
			}
		}
		var act actionConfig
		if len(row.Action) > 0 {
			if err := json.Unmarshal(row.Action, &act); err != nil {
				return nil, err
			}
		}

		rule := Rule{Type: row.Type, Reply: act.Reply, Handoff: act.Handoff}
		switch row.Type {
		case TypeKeyword:
			rule.Keywords = trig.Keywords
		case TypeOffHours:
			rule.Schedule = &Schedule{Timezone: trig.Timezone, Weekdays: trig.Weekdays, Start: trig.Start, End: trig.End}
		}
		out = append(out, rule)
	}
	return out, nil
}

// Evaluate returns the first matching rule's action, in rule order.
func Evaluate(rules []Rule, in Input) Decision {
	for _, r := range rules {
		if matches(r, in) {
			return Decision{Triggered: true, Reply: r.Reply, Handoff: r.Handoff}
		}
	}
	return Decision{}
}

func matches(r Rule, in Input) bool {
	switch r.Type {
	case TypeKeyword:
		text := strings.ToLower(in.Text)
		for _, kw := range r.Keywords {
			if kw != "" && strings.Contains(text, strings.ToLower(kw)) {
				return true
			}
		}
		return false
	case TypeWelcome:
		return in.FirstMessage
	case TypeOffHours:
		return r.Schedule != nil && !r.Schedule.isOpen(in.Now)
	default:
		return false
	}
}

func (s *Schedule) isOpen(now time.Time) bool {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t := now.In(loc)
	if len(s.Weekdays) > 0 && !slices.Contains(s.Weekdays, int(t.Weekday())) {
		return false
	}
	cur := t.Hour()*60 + t.Minute()
	start, okS := parseHM(s.Start)
	end, okE := parseHM(s.End)
	if !okS || !okE {
		return true // misconfigured window → treat as always open (don't block)
	}
	return cur >= start && cur < end
}

func parseHM(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}
