// Copyright 2017 Capsule8 Inc. All rights reserved.

package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"os"

	"github.com/capsule8/reactive8/pkg/api/event"
	"github.com/capsule8/reactive8/pkg/sensor"
	"github.com/golang/protobuf/proto"
	"github.com/kelseyhightower/envconfig"
	nats "github.com/nats-io/go-nats"
	stan "github.com/nats-io/go-nats-streaming"
)

type sensorConfig struct {
	StanClusterName     string `default:"c8-backplane" envconfig:"STAN_CLUSTERNAME"`
	NatsURL             string `default:"nats://localhost:4222" envconfig:"STAN_NATSURL"`
	SubscriptionTimeout int64  `default:"5"` // Default to a subscription timeout of 5 seconds
}

type subscriptionMetadata struct {
	lastSeen     int64 // Unix timestamp w/ second level precision of when sub was last seen
	subscription *event.Subscription
	stopChan     chan interface{}
}

var Config sensorConfig

// Map of subscription ID -> Subscription metadata
var subscriptions = make(map[string]*subscriptionMetadata)

func main() {
	log.Println("starting up")
	LoadConfig("sensor")
	StartSensor()
	log.Println("started")
	// Blocking call to remove stale subscriptions on a 5 second interval
	RemoveStaleSubscriptions()
}

// LoadConfig loads env vars into config with prefix `name`
func LoadConfig(name string) {
	err := envconfig.Process(name, &Config)
	if err != nil {
		log.Fatal("Failed to read env vars:", err)
	}
}

// StartSensor starts the async subscription listener
func StartSensor() {
	hostname, _ := os.Hostname()
	stanConn, err := stan.Connect(Config.StanClusterName, fmt.Sprintf("node-sensor_%s", hostname), stan.NatsURL(Config.NatsURL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't connect to STAN cluster: %v\n", err)
		os.Exit(1)
	}

	// Listen for subscriptions
	natsConn, err := nats.Connect(Config.NatsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to NATS: %v\n", err)
		os.Exit(1)
	}

	_, err = natsConn.Subscribe("subscription.*", func(m *nats.Msg) {
		subID := strings.Split(m.Subject, ".")[1]

		ss := &event.SignedSubscription{}
		if err = proto.Unmarshal(m.Data, ss); err != nil {
			fmt.Fprintf(os.Stderr, "No selector specified in subscription.%s\n", err.Error())
			return
		}

		// TODO: Filter subscriptions based on cluster/node information

		// Check if there is actually a `Selector` in the request. If not, ignore.
		if ss.Subscription.Selector == nil {
			fmt.Fprint(os.Stderr, "No selector specified in subscription.\n")
			return
		}

		// New subscription?
		if _, ok := subscriptions[subID]; !ok {
			subscriptions[subID] = &subscriptionMetadata{
				lastSeen:     time.Now().Add(time.Duration(Config.SubscriptionTimeout) * time.Second).Unix(),
				subscription: ss.Subscription,
			}
			subscriptions[subID].stopChan = newSensor(stanConn, ss.Subscription, subID)
		} else {
			// Existing subscription? Update unix ts
			subscriptions[subID].lastSeen = time.Now().Unix()
		}
	})
	if err != nil {
		log.Fatal("Failed to listen for new subscriptions:", err)
	}
}

// RemoveStaleSubscriptions is a blocking call that removes stale subscriptions @ `SubscriptionTimeout` interval
func RemoveStaleSubscriptions() {
	for {
		now := time.Now().Unix()
		for subscriptionID, subscription := range subscriptions {
			if now-subscription.lastSeen >= Config.SubscriptionTimeout {
				close(subscriptions[subscriptionID].stopChan)
				delete(subscriptions, subscriptionID)
			}

		}
		time.Sleep(time.Duration(Config.SubscriptionTimeout) * time.Second)
	}
}

func newSensor(conn stan.Conn, sub *event.Subscription, subscriptionID string) chan interface{} {
	stopChan := make(chan interface{})

	// Handle optional subscription arguments
	modifier := sub.Modifier
	if modifier == nil {
		modifier = &event.Modifier{}
	}
	stream, err := sensor.NewSensor(*sub.Selector, *modifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't start Sensor: %v\n", err)
		os.Exit(1)
	}

	go func() {
	sendLoop:
		for {
			select {
			// Stop the send loop
			case <-stopChan:
				break sendLoop
			case ev, ok := <-stream.Data:
				if !ok {
					fmt.Fprint(os.Stderr, "Failed to get next event.\n")
					break sendLoop
				}
				//log.Println("Sending event:", ev)

				data, err := proto.Marshal(ev.(*event.Event))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to marshal event data: %v\n", err)
				}
				// TODO: We should have some retry logic in place
				conn.PublishAsync(
					fmt.Sprintf("event.%s", subscriptionID),
					data,
					func(ackedNuid string, err error) {},
				)
			}
		}
	}()

	return stopChan
}
