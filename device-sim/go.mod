module seacontroll/firmware/device-sim

go 1.25.0

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	golang.org/x/net v0.54.0
	golang.org/x/sync v0.20.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace golang.org/x/net => golang.org/x/net v0.54.0

replace golang.org/x/sync => golang.org/x/sync v0.20.0
