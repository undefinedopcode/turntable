// procinfo is a standalone module: it depends on the turntable plugin SDK
// (resolved locally via replace while in this repo) and gopsutil. Keeping it a
// separate module means gopsutil never enters turntable's or the SDK's own
// dependency graph.
module github.com/april/turntable/examples/plugins/procinfo

go 1.24.0

require (
	github.com/april/turntable/sdk/go v0.0.0
	github.com/shirou/gopsutil/v4 v4.26.5
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

replace github.com/april/turntable/sdk/go => ../../../sdk/go
