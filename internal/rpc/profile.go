package rpc

import "time"

// MethodProfileCapture is a local operator diagnostic, not an MCP tool.
const MethodProfileCapture = "diagnostics.profile"

// ProfileMaxDuration bounds an explicitly requested diagnostic capture.
const ProfileMaxDuration = 2 * time.Minute

// ProfileParams selects a bounded process profile; no caller-supplied paths.
type ProfileParams struct {
	Kind    string `json:"kind"`
	Seconds int    `json:"seconds"`
}

// ProfileResult identifies private files written by the daemon being profiled.
type ProfileResult struct {
	Kind       string    `json:"kind"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Directory  string    `json:"directory"`
	Files      []string  `json:"files"`
}
