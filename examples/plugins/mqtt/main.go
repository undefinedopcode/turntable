// Command mqtt is a reference turntable plugin connector that snapshots an MQTT
// broker — how sensor fleets (water meters, SCADA gateways, home automation)
// actually move data. It is deliberately bounded: turntable is a pull-based,
// read-only query engine, so rather than streaming forever, each scan
// subscribes, collects for a fixed window, and returns what it saw as rows.
//
//	messages   one row per message received during the window:
//	           ts (time), topic (string), payload (string), qos (int),
//	           retained (bool)
//
// Retained messages arrive immediately on subscribe, so even a short window
// (the 2s default) snapshots every retained topic — the broker's "current
// state" — while a longer `duration` samples live traffic. Combine with the
// engine's JSON_EXTRACT for JSON payloads:
//
//	SELECT topic, JSON_EXTRACT(payload, '$.flow') AS flow, ts
//	FROM mq WHERE topic LIKE 'site/%'
//
// Options (all strings, set on the source):
//
//	broker     required — e.g. tcp://localhost:1883, ssl://host:8883, ws://…
//	topic      subscription filter (default "#")
//	duration   seconds to collect after subscribing (default 2)
//	qos        0, 1 or 2 (default 0)
//	client_id  MQTT client id (default "turntable-mqtt")
//	username   optional; use ${ENV_VAR} references in the config for
//	password   credentials so they never land in the file
//	max        cap on collected messages (default 10000)
//
// Register it (see PLUGINS.md):
//
//	sources:
//	  mq:
//	    connector: plugin
//	    command: ["./bin/mqtt"]
//	    options:
//	      dataset: messages
//	      broker: tcp://localhost:1883
//	      topic: "site/#"
//	      duration: "3"
package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/april/turntable/sdk/go/ttplugin"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	defaultDuration = 2 * time.Second
	defaultMax      = 10000
	connectTimeout  = 10 * time.Second
)

func main() {
	ttplugin.Serve(ttplugin.Plugin{
		Name: "mqtt",
		Datasets: map[string]ttplugin.Dataset{
			"messages": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "ts", Type: "time"},
					{Name: "topic", Type: "string"},
					{Name: "payload", Type: "string", Nullable: true},
					{Name: "qos", Type: "int"},
					{Name: "retained", Type: "bool"},
				}},
				Rows: collect,
			},
		},
	})
}

// collect subscribes and gathers messages for the configured window. The SDK
// applies the pushed-down WHERE/LIMIT to what we return.
func collect(req ttplugin.Request) (ttplugin.Rows, error) {
	broker := strOpt(req.Options, "broker")
	if broker == "" {
		return nil, fmt.Errorf("mqtt plugin needs a broker option (e.g. tcp://localhost:1883)")
	}
	topic := strOpt(req.Options, "topic")
	if topic == "" {
		topic = "#"
	}
	window := defaultDuration
	if s := strOpt(req.Options, "duration"); s != "" {
		sec, err := strconv.ParseFloat(s, 64)
		if err != nil || sec <= 0 {
			return nil, fmt.Errorf("duration must be a positive number of seconds")
		}
		window = time.Duration(sec * float64(time.Second))
	}
	qos := byte(0)
	if s := strOpt(req.Options, "qos"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 2 {
			return nil, fmt.Errorf("qos must be 0, 1 or 2")
		}
		qos = byte(n)
	}
	maxMsgs := defaultMax
	if s := strOpt(req.Options, "max"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("max must be a positive integer")
		}
		maxMsgs = n
	}
	clientID := strOpt(req.Options, "client_id")
	if clientID == "" {
		clientID = "turntable-mqtt"
	}

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetConnectTimeout(connectTimeout).
		SetAutoReconnect(false) // one bounded snapshot; no reconnect loops

	if u := strOpt(req.Options, "username"); u != "" {
		opts.SetUsername(u)
	}
	if p := strOpt(req.Options, "password"); p != "" {
		opts.SetPassword(p)
	}

	var (
		mu   sync.Mutex
		rows ttplugin.Rows
		full = make(chan struct{})
		once sync.Once
	)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		if len(rows) >= maxMsgs {
			once.Do(func() { close(full) })
			return
		}
		rows = append(rows, ttplugin.Row{
			time.Now().UTC(),
			m.Topic(),
			string(m.Payload()),
			int(m.Qos()),
			m.Retained(),
		})
	})

	client := mqtt.NewClient(opts)
	if tok := client.Connect(); !tok.WaitTimeout(connectTimeout) || tok.Error() != nil {
		return nil, fmt.Errorf("mqtt connect %s: %v", broker, tokenErr(tok))
	}
	defer client.Disconnect(250)

	if tok := client.Subscribe(topic, qos, nil); !tok.WaitTimeout(connectTimeout) || tok.Error() != nil {
		return nil, fmt.Errorf("mqtt subscribe %q: %v", topic, tokenErr(tok))
	}

	// Collect until the window elapses or the message cap fills.
	select {
	case <-time.After(window):
	case <-full:
	}

	mu.Lock()
	defer mu.Unlock()
	return rows, nil
}

func tokenErr(tok mqtt.Token) error {
	if err := tok.Error(); err != nil {
		return err
	}
	return fmt.Errorf("timeout")
}

func strOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
