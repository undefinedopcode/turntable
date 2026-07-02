// mqtt is a standalone module (like any real external plugin would be), so the
// paho MQTT client never enters turntable's or the SDK's dependency graph. It
// depends on the turntable plugin SDK, resolved locally via the replace
// directive while in this repo.
module github.com/april/turntable/examples/plugins/mqtt

go 1.23

require (
	github.com/undefinedopcode/turntable-go-sdk v0.0.0
	github.com/eclipse/paho.mqtt.golang v1.5.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.27.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
)

replace github.com/undefinedopcode/turntable-go-sdk => ../../../sdk/go
