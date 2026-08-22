module github.com/osauer/canary/v2

go 1.27.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ProtonMail/go-crypto v1.4.1
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/coder/websocket v1.8.14
	github.com/osauer/hyperserve v1.2.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/yuin/goldmark v1.7.17
	golang.org/x/mod v0.37.0
	golang.org/x/sys v0.46.0
	modernc.org/sqlite v1.54.0
)

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/telemetry v0.0.0-20260625142307-59b4966ccb57 // indirect
	golang.org/x/time v0.7.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	golang.org/x/tools/gopls v0.21.1 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

tool (
	golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
