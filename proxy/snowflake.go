package main

import (
	"flag"
	"io"
	"log"
	"os"

	"git.torproject.org/pluggable-transports/snowflake.git/common/safelog"

	sf "git.torproject.org/pluggable-transports/snowflake.git/proxy/lib"
)

const defaultBrokerURL = "https://snowflake-broker.bamsoftware.com/"
const defaultRelayURL = "wss://snowflake.bamsoftware.com/"
const defaultSTUNURL = "stun:stun.stunprotocol.org:3478"

func main() {
	var capacity uint
	var relayURL string
	var stunURL string
	var logFilename string
	var rawBrokerURL string
	var unsafeLogging bool
	var keepLocalAddresses bool

	flag.UintVar(&capacity, "capacity", 10, "maximum concurrent clients")
	flag.StringVar(&rawBrokerURL, "broker", defaultBrokerURL, "broker URL")
	flag.StringVar(&relayURL, "relay", defaultRelayURL, "websocket relay URL")
	flag.StringVar(&stunURL, "stun", defaultSTUNURL, "stun URL")
	flag.StringVar(&logFilename, "log", "", "log filename")
	flag.BoolVar(&unsafeLogging, "unsafe-logging", false, "prevent logs from being scrubbed")
	flag.BoolVar(&keepLocalAddresses, "keep-local-addresses", false, "keep local LAN address ICE candidates")
	flag.Parse()

	var logOutput io.Writer = os.Stderr
	log.SetFlags(log.LstdFlags | log.LUTC)
	if logFilename != "" {
		f, err := os.OpenFile(logFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		logOutput = io.MultiWriter(os.Stderr, f)
	}
	if unsafeLogging {
		log.SetOutput(logOutput)
	} else {
		// We want to send the log output through our scrubber first
		log.SetOutput(&safelog.LogScrubber{Output: logOutput})
	}

	log.Println("starting")

	err := sf.Proxy(capacity, rawBrokerURL, relayURL, stunURL, keepLocalAddresses)
	if err != nil {
		log.Printf("Error proxying Snowflake traffic: %s", err.Error())
	}
	log.Printf("stopping")
}
