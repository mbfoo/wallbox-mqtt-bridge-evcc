package main

import (
	"os"

	bridge "github.com/mbfoo/wallbox-mqtt-bridge-evcc/app"
)

func main() {
	if len(os.Args) != 2 {
		panic("Usage: ./bridge --config or ./bridge bridge.ini")
	}
	firstArgument := os.Args[1]
	if firstArgument == "--config" {
		bridge.RunTuiSetup()
	} else {
		bridge.RunBridge(firstArgument)
	}
}
