package bridge

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/mbfoo/wallbox-mqtt-bridge-evcc/app/wallbox"
)

const (
	mqttPublishTimeout    = 2 * time.Second
	mqttReconnectInterval = 10 * time.Second
	mqttInitialRetryDelay = 5 * time.Second
)

func RunBridge(configPath string) {
	c := LoadConfig(configPath)
	if c.Settings.PollingIntervalSeconds <= 0 {
		fmt.Println("Warning: polling_interval_seconds is unset or zero; defaulting to 1s")
		c.Settings.PollingIntervalSeconds = 1
	}

	w := wallbox.New()
	w.IncludePowerBoost = c.Settings.PowerBoostEnabled

	w.RefreshData()

	w.StartRedisSubscriptions()
	defer w.StopRedisSubscriptions()

	// serialNumber, firmwareVersion, and partNumber are captured once here and
	// baked into all MQTT topics and HA discovery configs for the process
	// lifetime. If serialNumber is empty, every topic is broken and no
	// reconnect can fix it — so retry until we get a valid value, the same
	// way we retry the initial MQTT connect below.
	var serialNumber, firmwareVersion, partNumber string
	for {
		serialNumber = w.SerialNumber()
		firmwareVersion = w.FirmwareVersion()
		partNumber = w.PartNumber()
		if serialNumber != "" {
			break
		}
		fmt.Println("Warning: serial number read returned empty — retrying in 5s")
		time.Sleep(5 * time.Second)
	}

	entityConfig := getEntities(w)
	if c.Settings.DebugSensors {
		maps.Copy(entityConfig, getDebugEntities(w))
	}
	if c.Settings.PowerBoostEnabled {
		maps.Copy(entityConfig, getPowerBoostEntities(w))
	}

	topicPrefix := "wallbox_" + serialNumber
	availabilityTopic := topicPrefix + "/availability"

	// published tracks the last-published value per entity so we only push
	// to MQTT when something actually changed. A mutex guards it because the
	// onConnect handler (called from the paho goroutine) clears it while the
	// main loop reads/writes it.
	published := make(map[string]any)
	lastUpdateIntervalUpdatedTopics := make(map[string]time.Time)
	var publishedMu sync.Mutex

	messageHandler := func(client mqtt.Client, msg mqtt.Message) {
		parts := strings.Split(msg.Topic(), "/")
		if len(parts) < 2 {
			return
		}
		field := parts[1]
		entity, ok := entityConfig[field]
		if !ok || entity.Setter == nil {
			// Ignore set commands for unknown or read-only entities.
			// This can happen with stale retained messages from a previous
			// install, or a misconfigured HA automation.
			fmt.Println("Warning: ignoring set command for unknown/read-only entity:", field)
			return
		}
		payload := string(msg.Payload())
		fmt.Println("Setting", field, payload)
		entity.Setter(payload)
	}

	// onConnect is called by paho on every successful connection, including
	// automatic reconnections. It re-publishes all HA discovery configs,
	// re-subscribes to command topics, and marks the device online.
	// It also clears the published-state cache so the main loop will
	// immediately re-push all current sensor values to the (re)connected
	// broker — critical when the broker itself restarted and lost retained state.
	onConnect := func(client mqtt.Client) {
		fmt.Println("Connected to MQTT broker. Publishing discovery configs...")

		for key, val := range entityConfig {
			component := val.Component
			uid := serialNumber + "_" + key
			config := map[string]any{
				"~":                  topicPrefix + "/" + key,
				"availability_topic": availabilityTopic,
				"state_topic":        "~/state",
				"unique_id":          uid,
				"device": map[string]string{
					"identifiers":  serialNumber,
					"name":         c.Settings.DeviceName,
					"model":        partNumber,
					"manufacturer": "Wallbox",
					"sw_version":   firmwareVersion,
				},
			}
			if val.Setter != nil {
				config["command_topic"] = "~/set"
			}
			for k, v := range val.Config {
				config[k] = v
			}
			jsonPayload, _ := json.Marshal(config)
			client.Publish("homeassistant/"+component+"/"+uid+"/config", 1, true, jsonPayload)
		}

		// Re-establish command topic subscriptions.
		client.Subscribe(topicPrefix+"/+/set", 1, messageHandler)

		// Mark device online.
		client.Publish(availabilityTopic, 1, true, "online")

		// Clear published cache so all current values are re-pushed on the
		// next tick. Without this, a broker restart causes HA to show stale
		// or missing sensor state because the broker's retained messages are
		// gone and the main loop thinks there's nothing new to send.
		publishedMu.Lock()
		for k := range published {
			delete(published, k)
		}
		publishedMu.Unlock()
	}

	opts := mqtt.NewClientOptions()
	brokerURL := c.MQTT.URL
	if brokerURL == "" {
		brokerURL = fmt.Sprintf("tcp://%s:%d", c.MQTT.Host, c.MQTT.Port)
	}
	opts.AddBroker(brokerURL)
	opts.SetUsername(c.MQTT.Username)
	opts.SetPassword(c.MQTT.Password)
	opts.SetWill(availabilityTopic, "offline", 1, true)

	// Enable paho's built-in reconnect logic so a transient broker outage
	// (HA update, network blip) never crashes the bridge.
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(mqttReconnectInterval)
	opts.SetOnConnectHandler(onConnect)
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		fmt.Printf("MQTT connection lost: %v — reconnecting automatically\n", err)
	})

	client := mqtt.NewClient(opts)

	// Retry the initial connection in a loop rather than panicking; the broker
	// may be temporarily unavailable at startup (e.g. HA is still booting).
	for {
		token := client.Connect()
		token.Wait()
		if token.Error() == nil {
			break
		}
		fmt.Printf("Initial MQTT connect failed: %v — retrying in %s\n", token.Error(), mqttInitialRetryDelay)
		time.Sleep(mqttInitialRetryDelay)
	}

	ticker := time.NewTicker(time.Duration(c.Settings.PollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	// publishChanged iterates every entity, evaluates its getter, and publishes
	// to MQTT if the value has changed since the last publish — subject to the
	// entity's rate limiter. It is called both on ticker ticks (after a full
	// SQL+Redis refresh) and on live notifications (no SQL refresh).
	publishChanged := func() {
		// Don't attempt publishes while disconnected; paho is reconnecting in
		// the background and onConnect will flush everything on reconnect.
		if !client.IsConnected() {
			return
		}

		publishedMu.Lock()
		defer publishedMu.Unlock()

		for key, val := range entityConfig {
			payload := val.Getter()
			isIntervalTopic := slices.Index(c.Settings.IntervalUpdatedTopics, key) > -1
			isIntervalDue := isUpdateForTopicNecessary(lastUpdateIntervalUpdatedTopics[key], c)
			if c.Settings.VerboseOutput {
				fmt.Printf("Check publishing '%s': isIntervalTopic=%t, isIntervalDue=%t, lastUpdate=%s\n",
					key, isIntervalTopic, isIntervalDue, lastUpdateIntervalUpdatedTopics[key].String())
			}
			if published[key] == payload && !(isIntervalTopic && isIntervalDue) {
				continue
			}
			if val.RateLimit != nil && !val.RateLimit.Allow(strToFloat(payload)) {
				if c.Settings.VerboseOutput {
					fmt.Printf("Rate limit exceeded for key '%s' and payload '%s'\n", key, payload)
				}
				continue
			}
			fmt.Println("Publishing:", key, payload)
			token := client.Publish(topicPrefix+"/"+key+"/state", 1, true, []byte(payload))

			go func(t mqtt.Token, k string) {
				if !t.WaitTimeout(mqttPublishTimeout) {
					fmt.Println("Warning: publish timed out for", k)
				}
			}(token, key)

			published[key] = payload
			lastUpdateIntervalUpdatedTopics[key] = time.Now()
		}
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			// Full refresh
			w.RefreshData()
			publishChanged()

		case <-w.Updates:
			// Instant Real-Time loop: pub/sub event arrived
			publishChanged()

		case <-interrupt:
			fmt.Println("Interrupted. Exiting...")
			token := client.Publish(availabilityTopic, 1, true, "offline")
			token.WaitTimeout(mqttPublishTimeout)
			client.Disconnect(250)
			return
		}
	}
}

func isUpdateForTopicNecessary(lastUpdate time.Time, config *WallboxConfig) bool {
    if lastUpdate.IsZero() {
        return true
    }
    return int(time.Since(lastUpdate).Seconds()) > config.Settings.IntervalUpdatedTopicsSeconds
}