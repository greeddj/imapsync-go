module github.com/greeddj/imapsync-go

go 1.27.4

require (
	github.com/emersion/go-imap v1.2.1
	github.com/jedib0t/go-pretty/v6 v6.9.1
	github.com/urfave/cli/v3 v3.14.0
	golang.org/x/sync v0.25.0
	golang.org/x/term v0.48.0
	golang.org/x/time v0.16.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/exp/typeparams v0.0.0-20261113113407-e3970619a1a4 // indirect
	golang.org/x/mod v0.43.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/telemetry v0.0.0-20261111153349-cdaf7e97dd7b // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.9.0 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
