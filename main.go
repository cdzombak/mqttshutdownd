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
	"github.com/google/cel-go/cel"
)

const name = "mqttshutdownd"

var version = "<dev>"

func usage() {
	fmt.Fprintf(os.Stderr, "mqttshutdownd %s\n", version)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "mqttshutdownd subscribes to an MQTT topic and initiates a system shutdown when a message is received indicating that utility power is down.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "-down-expr and -recovered-expr are Common Experssion Language (CEL) expressions. For more information on CEL, see https://cel.dev .")
	fmt.Fprintln(os.Stderr, "Within those expressions, the following variables are available:")
	fmt.Fprintln(os.Stderr, "  - powerType: integer, representing the type of power event received from MQTT (e.g. 1 = utility power)")
	fmt.Fprintln(os.Stderr, "  - online: boolean, representing whether the power type is online")
	fmt.Fprintln(os.Stderr, "  - scope: string, representing the scope of the power event (e.g. 'global')")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "mqttshutdownd is licensed under the LGPL-3.0 license.")
	fmt.Fprintln(os.Stderr, "https://www.github.com/cdzombak/mqttshutdownd")
	fmt.Fprintln(os.Stderr, "by Chris Dzombak <https://www.dzombak.com>")
}

func main() {
	topic := flag.String("topic", "", "MQTT topic to subscribe to. Required.")
	server := flag.String("server", "", "MQTT server and port to connect to, e.g. 'mymqttserver.lan:1883'. Required.")
	user := flag.String("user", "", "MQTT username.")
	password := flag.String("password", "", "MQTT password.")
	sessionExpiryS := flag.Int("session-expiry", 5*60, "Seconds that a session will survive after disconnection for delivery of QoS 1/2 messages.")
	recoveryPeriod := flag.Duration("recovery-period", 3*time.Minute, "Duration to wait after utility power is lost before initiating shutdown.")
	downExpr := flag.String("down-expr", "!online && powerType == 1", "CEL expression determining whether an event should trigger a shutdown.")
	recoveredExpr := flag.String("recovered-expr", "online && powerType == 1", "CEL expression determining whether an event should cancel a pending shutdown.")
	debug := flag.Bool("debug", false, "Enable debug-level logging.")
	strict := flag.Bool("strict", false, "Exit on invalid messages or unexpected topics.")
	printVersion := flag.Bool("version", false, "Print version, then exit.")
	helpSystemdUsage := flag.Bool("help-systemd-usage", false, "Print instructions on configuring the systemd unit, then exit.")
	flag.Usage = usage
	flag.Parse()

	if *printVersion {
		fmt.Printf("%s %s\n", name, version)
		os.Exit(0)
	}

	if *helpSystemdUsage {
		fmt.Fprintln(os.Stderr, "To use the mqttd systemd service, you must customize the service file via:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  sudo systemctl edit mqttshutdownd.service")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Customize the [Service] ExecStart line to include the desired arguments.")
		fmt.Fprintln(os.Stderr, "For example, to set the MQTT server and topic (the minimal required arguments), add the following to your edit:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  [Service]")
		fmt.Fprintln(os.Stderr, "  ExecStart=")
		fmt.Fprintln(os.Stderr, "  ExecStart=/usr/bin/mqttshutdownd -server mymqttserver.lan:1883 -topic power/alarms")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "(Both ExecStart= lines are required; see https://stackoverflow.com/a/68818218 )")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "After saving and closing the editor, reload systemd and restart the service:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  sudo systemctl daemon-reload")
		fmt.Fprintln(os.Stderr, "  sudo systemctl restart mqttshutdownd")
		os.Exit(6) // EXIT_NOTCONFIGURED
	}

	if *topic == "" {
		fmt.Fprintln(os.Stderr, "-topic is required.")
		fmt.Fprintln(os.Stderr, "")
		usage()
		os.Exit(2) // EXIT_INVALIDARGUMENT
	}
	if *server == "" {
		fmt.Fprintln(os.Stderr, "-server is required.")
		fmt.Fprintln(os.Stderr, "")
		usage()
		os.Exit(2) // EXIT_INVALIDARGUMENT
	}
	if *sessionExpiryS < 0 {
		fmt.Fprintln(os.Stderr, "-session-expiry must be an unsigned 32 bit integer.")
		fmt.Fprintln(os.Stderr, "")
		usage()
		os.Exit(2) // EXIT_INVALIDARGUMENT
	}

	strictLog := StrictLogger(*strict)
	debugLog := DebugLogger(*debug)

	const (
		celVarPowerType = "powerType"
		celVarOnline    = "online"
		celVarScope     = "scope"
	)
	celEnv, err := cel.NewEnv(
		cel.Variable(celVarPowerType, cel.IntType),
		cel.Variable(celVarOnline, cel.BoolType),
		cel.Variable(celVarScope, cel.StringType),
	)
	if err != nil {
		log.Fatalf("failed to create CEL environment: %s", err)
	}
	downExprAst, iss := celEnv.Compile(*downExpr)
	if iss.Err() != nil {
		log.Fatalf("failed to compile -down-expr '%s': %s", *downExpr, iss.Err())
	}
	if downExprAst.OutputType() != cel.BoolType {
		log.Fatalf("-down-expr '%s' does not return a boolean", *recoveredExpr)
	}
	downExprPrg, err := celEnv.Program(downExprAst)
	if err != nil {
		log.Fatalf("failed to generate program for -down-expr '%s': %s", *downExpr, err)
	}
	recoveredExprAst, iss := celEnv.Compile(*recoveredExpr)
	if iss.Err() != nil {
		log.Fatalf("failed to compile -recovered-expr '%s': %s", *recoveredExpr, iss.Err())
	}
	if recoveredExprAst.OutputType() != cel.BoolType {
		log.Fatalf("-recovered-expr '%s' does not return a boolean", *recoveredExpr)
	}
	recoveredExprPrg, err := celEnv.Program(recoveredExprAst)
	if err != nil {
		log.Fatalf("failed to generate program for -recovered-expr '%s': %s", *recoveredExpr, err)
	}

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

					if t == nil {
						out, _, err := downExprPrg.Eval(map[string]any{
							celVarScope:     m.Scope,
							celVarPowerType: m.PowerType,
							celVarOnline:    m.Online,
						})
						if err != nil {
							log.Fatalf("failed to evaluate -down-expr: %s", err)
						}
						triggerShutdown := out.Value().(bool)
						if triggerShutdown {
							log.Printf("power down; shutdown in %s", *recoveryPeriod)
							t = time.AfterFunc(*recoveryPeriod, func() {
								log.Println("calling shutdown!")
								err := exec.Command("shutdown", "-h", "now").Run()
								if err != nil {
									log.Fatalf("failed to call shutdown: %s", err)
								}
								log.Println("shutdown initiated!")
							})
						}
					} else {
						out, _, err := recoveredExprPrg.Eval(map[string]any{
							celVarScope:     m.Scope,
							celVarPowerType: m.PowerType,
							celVarOnline:    m.Online,
						})
						if err != nil {
							log.Fatalf("failed to evaluate -recovered-expr: %s", err)
						}
						triggerRecovery := out.Value().(bool)
						if triggerRecovery {
							log.Println("power recovered; cancelling pending shutdown")
							t.Stop()
							t = nil
						}
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
