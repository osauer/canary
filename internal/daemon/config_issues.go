package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// configAutomationSections are the config.toml sections whose values shape
// pre-authorised submission: the order limits, the protection proposal
// settings, and the Rulebook policy a budget basis can read. While one of
// them runs on Canary's defaults, pre-authorised submission pauses; manual
// exits, trims and reads continue (owner decision 2026-09-26).
var configAutomationSections = []string{"trading", "auto_trade", "rulebook"}

func (s *Server) addConfigIssue(issue config.Issue) {
	if s == nil {
		return
	}
	s.configIssuesMu.Lock()
	defer s.configIssuesMu.Unlock()
	s.configIssues = append(s.configIssues, issue)
	if s.configIssuesAt.IsZero() {
		s.configIssuesAt = time.Now().UTC()
	}
}

// configIssuesSnapshot returns the parts of config.toml running on Canary's
// defaults and when they were read.
func (s *Server) configIssuesSnapshot() ([]config.Issue, time.Time) {
	if s == nil {
		return nil, time.Time{}
	}
	s.configIssuesMu.Lock()
	defer s.configIssuesMu.Unlock()
	return slices.Clone(s.configIssues), s.configIssuesAt
}

// configPausesAutomation reports whether a section that shapes pre-authorised
// submission runs on Canary's defaults, naming those sections.
func (s *Server) configPausesAutomation() (bool, []string) {
	issues, _ := s.configIssuesSnapshot()
	var sections []string
	for _, issue := range issues {
		if section := issue.Section(); slices.Contains(configAutomationSections, section) && !slices.Contains(sections, section) {
			sections = append(sections, section)
		}
	}
	return len(sections) > 0, sections
}

// configIssueSummary is the one sentence every surface quotes.
func (s *Server) configIssueSummary() string {
	issues, _ := s.configIssuesSnapshot()
	if len(issues) == 0 {
		return ""
	}
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		keys = append(keys, issue.Key)
	}
	out := fmt.Sprintf("%d part(s) of config.toml could not be read and run on Canary's defaults: %s", len(issues), strings.Join(keys, ", "))
	if paused, sections := s.configPausesAutomation(); paused {
		out += fmt.Sprintf("; pre-authorised submission is paused while [%s] runs on defaults, and manual exits, trims and reads continue", strings.Join(sections, "], ["))
	}
	return out + ". Fix the file and run `canary restart`"
}

// configSubsystemHealth reports config.toml on status: ready when every part
// read, degraded while any part runs on Canary's defaults.
func (s *Server) configSubsystemHealth() rpc.SubsystemHealth {
	issues, at := s.configIssuesSnapshot()
	if len(issues) == 0 {
		return rpc.SubsystemHealth{Name: "config", Status: "ready"}
	}
	details := make([]string, 0, len(issues))
	for _, issue := range issues {
		details = append(details, issue.String())
	}
	return rpc.SubsystemHealth{Name: "config", Status: "degraded", Message: s.configIssueSummary(),
		LastError: strings.Join(details, "; "), LastErrorAt: at}
}

// briefConfigRow is the brief's Ready row for config.toml; nil while every
// part reads, so a clean file adds no row.
func (s *Server) briefConfigRow() *rpc.BriefConfigRow {
	issues, _ := s.configIssuesSnapshot()
	if len(issues) == 0 {
		return nil
	}
	row := &rpc.BriefConfigRow{BriefRowState: briefAttention(s.configIssueSummary())}
	for _, issue := range issues {
		row.Issues = append(row.Issues, issue.String())
	}
	row.AutomationPaused, _ = s.configPausesAutomation()
	return row
}
