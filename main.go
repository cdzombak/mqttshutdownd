package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

const name = "mqttshutdownd"

var version = "<dev>"

//goland:noinspection GoUnusedConst
const (
	PowerTypeUtility   = 1
	PowerTypeGenerator = 2
	PowerTypeBattery   = 3
	PowerTypeSolar     = 4

	ScopeGlobal      = "global"
	ScopeSinglePhase = "1p"
	ScopeOneCircuit  = "1c"
)

// PowerAlarmMessage is the shape of messages on the power/alarms topic.
type PowerAlarmMessage struct {
	Online    bool   `json:"up"`
	PowerType int    `json:"type"`
	Scope     string `json:"scope"`
}

func (p *PowerAlarmMessage) Valid() bool {
	if p.PowerType < PowerTypeUtility || p.PowerType > PowerTypeSolar {
		return false
	}
	if p.Scope != ScopeGlobal && p.Scope != ScopeSinglePhase && p.Scope != ScopeOneCircuit {
		return false
	}
	return true
}

func StrictLogger(strict bool) func(m string) {
	if strict {
		return func(m string) {
			log.Fatal(m)
		}
	} else {
		return func(m string) {
			log.Println(m)
		}
	}
}

func DebugLogger(debug bool) func(m string) {
	if debug {
		return func(m string) {
			log.Printf("[DEBUG] %s", m)
		}
	} else {
		return func(m string) {}
	}
}

// TODO(cdzombak): allow more specific policy configuration based on type & scope/
//                 right now this program initiates shutdown as long as any scope of utility power is down
//                 and does not recover within a configurable period.

func main() {
	topic := flag.String("topic", "", "MQTT topic to subscribe to. Required.")
	server := flag.String("server", "", "MQTT server and port to connect to, e.g. 'mymqttserver.lan:1883'. Required.")
	user := flag.String("user", "", "MQTT username.")
	password := flag.String("password", "", "MQTT password.")
	sessionExpiryS := flag.Int("session-expiry", 5*60, "Seconds that a session will survive after disconnection for delivery of QoS 1/2 messages.")
	recoveryPeriod := flag.Duration("recovery-period", 3*time.Minute, "Duration to wait after utility power is lost before initiating shutdown.")
	debug := flag.Bool("debug", false, "Enable debug-level logging.")
	strict := flag.Bool("strict", false, "Exit on invalid messages or unexpected topics.")
	printVersion := flag.Bool("version", false, "Print version and exit.")
	helpSystemdUsage := flag.Bool("help-systemd-usage", false, "")
	flag.Parse()

	if *printVersion {
		fmt.Printf("%s %s\n", name, version)
		os.Exit(0)
	}

	if *helpSystemdUsage {
		fmt.Println("To use the mqttd systemd service, you must customize the service file via:")
		fmt.Println("  sudo systemctl edit mqttdshutdownd.service")
		fmt.Println("Customize the [Service] ExecStart line to include the desired arguments.")
		fmt.Println("For example, to set the MQTT server and topic (the minimal required arguments), add the following to your edit:")
		fmt.Println("  [Service]")
		fmt.Println("  ExecStart=/usr/bin/mqttshutdownd -server mymqttserver.lan:1883 -topic power/alarms")
		os.Exit(6) // EXIT_NOTCONFIGURED
	}

	if *topic == "" {
		log.Fatal("-topic is required.")
	}
	if *server == "" {
		log.Fatal("-server is required.")
	}
	if *sessionExpiryS < 0 {
		log.Fatalf("-session-expiry must be an unsigned 32 bit integer.")
	}

	strictLog := StrictLogger(*strict)
	debugLog := DebugLogger(*debug)

	serverURL, err := url.Parse(fmt.Sprintf("mqtt://%s", *server))
	if err != nil {
		log.Fatalf("failed to parse server URL 'mqtt://%s': %s", *server, err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("failed to get hostname: %s", err)
	}
	clientID := fmt.Sprintf("%s/%s", hostname, name)
	log.Printf("generated client ID: %s", clientID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	receivedMessages := make(chan paho.PublishReceived)
	go func(ctx context.Context) {
		var (
			t   *time.Timer
			tMu sync.Mutex
		)
		for {
			select {
			case <-ctx.Done():
				return
			case rm := <-receivedMessages:
				// should never happen; can't hurt to check:
				if rm.Packet.Topic != *topic {
					strictLog(fmt.Sprintf("received message on unexpected topic: %s", rm.Packet.Topic))
					continue
				}
				var m PowerAlarmMessage
				if err := json.Unmarshal(rm.Packet.Payload, &m); err != nil {
					strictLog(fmt.Sprintf("failed to unmarshal message: %s\n(content: '%s')", err, rm.Packet.Payload))
					continue
				}
				if !m.Valid() {
					strictLog(fmt.Sprintf("invalid message schema: '%s'", rm.Packet.Payload))
					continue
				}
				func() {
					tMu.Lock()
					defer tMu.Unlock()
					// TODO(cdzombak): additional policy logic goes here
					if m.PowerType == PowerTypeUtility && !m.Online && t == nil {
						log.Printf("utility power down; shutdown in %s", *recoveryPeriod)
						t = time.AfterFunc(*recoveryPeriod, func() {
							log.Println("calling shutdown!")
							err := exec.Command("shutdown", "-h", "now").Run()
							if err != nil {
								log.Fatalf("failed to call shutdown: %s", err)
							}
							log.Println("shutdown initiated!")
						})
					}
					if m.PowerType == PowerTypeUtility && m.Online && t != nil {
						log.Println("utility power restored; cancelling pending shutdown")
						t.Stop()
						t = nil
					}
				}()
			}
		}
	}(ctx)

	cliCfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{serverURL},
		ConnectUsername:               *user,
		ConnectPassword:               []byte(*password),
		KeepAlive:                     20,
		CleanStartOnInitialConnection: false,
		SessionExpiryInterval:         uint32(*sessionExpiryS),
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			log.Printf("connected to '%s'", *server)
			// Subscribing in the OnConnectionUp callback is recommended (ensures the subscription is reestablished if the connection drops)
			if _, err := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{{Topic: *topic, QoS: 1}},
			}); err != nil {
				log.Fatalf("failed to subscribe to topic '%s': %s", *topic, err)
			}
			log.Printf("subscribed to '%s'", *topic)
		},
		OnConnectError: func(err error) {
			log.Printf("error while attempting connection: %s", err)
		},
		// eclipse/paho.golang/paho provides base mqtt functionality, the below config will be passed in for each connection
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					debugLog(fmt.Sprintf("received message on topic %s; body: %s (retain: %t)", pr.Packet.Topic, pr.Packet.Payload, pr.Packet.Retain))
					receivedMessages <- pr
					return true, nil
				}},
			OnClientError: func(err error) {
				log.Fatalf("client error: %s", err)
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					log.Fatalf("server requested disconnect: %s\n", d.Properties.ReasonString)
				} else {
					log.Fatalf("server requested disconnect; reason code: %d\n", d.ReasonCode)
				}
			},
		},
	}
	c, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		log.Fatalf("failed to start connection: %s", err)
	}

	<-c.Done()
	log.Println("signal caught - exiting")
}
