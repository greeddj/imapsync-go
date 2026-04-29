module github.com/greeddj/imapsync-go

go 1.26

require (
	github.com/emersion/go-imap v1.2.1
	github.com/jedib0t/go-pretty/v6 v6.7.10
	github.com/urfave/cli/v3 v3.8.0
	golang.org/x/term v0.42.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/telemetry v0.0.0-20260428171046-76f71b9afea0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
