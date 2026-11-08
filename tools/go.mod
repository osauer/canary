module github.com/osauer/canary/v2/tools

go 1.27.3

require golang.org/x/tools v0.52.0

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/telemetry v0.0.0-20261010193011-f71dc92da45e // indirect
	golang.org/x/vuln v1.9.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize
	golang.org/x/vuln/cmd/govulncheck
	google.golang.org/protobuf/cmd/protoc-gen-go
	honnef.co/go/tools/cmd/staticcheck
)
