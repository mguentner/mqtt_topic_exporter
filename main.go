//#!/usr/bin/env go run
package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"encoding/json"

	"github.com/satori/go.uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"github.com/yosssi/gmq/mqtt"
	"github.com/yosssi/gmq/mqtt/client"
	"gopkg.in/alecthomas/kingpin.v2"
)

// https://github.com/prometheus/node_exporter/blob/master/node_exporter.go

var topicLastHandled = map[string]time.Time{}
var topicLastHandledMutex = new(sync.Mutex)

var namespace = "mqtt"

var mqttGauge *prometheus.GaugeVec

func init() {
	mqttGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: fmt.Sprintf("%s_topic", namespace),
		Help: "mqtt topic gauge",
	}, []string{"topic"})

	prometheus.MustRegister(version.NewCollector("mqtt_exporter"))
	prometheus.MustRegister(mqttGauge)
}

func main() {
	var (
		listenAddress = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").Default(":9981").String()
		metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		retainTimeStr = kingpin.Flag("mqtt.retain-time", "Retain duration for a topic").Default("1m").String()
		mqttServerUri = kingpin.Flag("mqtt.server", "MQTT Server address URI mqtts://user:pass@host:port").Required().URL()
		mqttTopics    = kingpin.Flag("mqtt.topic", "Watch MQTT topic").Required().Strings()
	)
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("mqtt_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting mqtt_topic_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	// parse retain time
	retainTime, err := time.ParseDuration(*retainTimeStr)
	if err != nil {
		log.Fatalf("specified %s is invalid", retainTimeStr)
	}

	// parse uri
	var tlsConfig *tls.Config
	if (*mqttServerUri).Scheme == "mqtts" {
		tlsConfig = &tls.Config{}
	}
	username := (*mqttServerUri).User.Username()
	password, _ := (*mqttServerUri).User.Password()
	newUuid, _ := uuid.NewV4()
	clientId := newUuid.String()

	log.Infof("value ratain: %d", retainTime)

	go func() {
		for {
			log.Infof("Connecting %s with topic %s", (*mqttServerUri).String(), strings.Join(*mqttTopics, " "))

			errChan := make(chan error)
			defer close(errChan)

			cli := client.New(&client.Options{
				ErrorHandler: func(err error) {
					errChan <- err
				},
			})
			defer cli.Terminate()

			err = cli.Connect(&client.ConnectOptions{
				Network:   "tcp",
				TLSConfig: tlsConfig,
				Address:   (*mqttServerUri).Host,
				UserName:  []byte(username),
				Password:  []byte(password),
				ClientID:  []byte(clientId),
			})
			if err != nil {
				log.Fatal(err)
			}

			// Subscribe to topics.
			for _, mqttTopic := range *mqttTopics {
				log.Infof("Subscribe topic %s", mqttTopic)
				err := cli.Subscribe(&client.SubscribeOptions{
					SubReqs: []*client.SubReq{
						&client.SubReq{
							TopicFilter: []byte(mqttTopic),
							QoS:         mqtt.QoS0,
							Handler: func(topicName, message []byte) {
								// mqtt_topic{topic="/foo/bar"} value
								topic := string(topicName)
								topicLastHandledMutex.Lock()
								topicLastHandled[topic] = time.Now()
								topicLastHandledMutex.Unlock()

								// flukso gives "[timestamp, watt, "W"]"
								var message_arr []float64
								_ = json.Unmarshal(message, &message_arr)

								value := message_arr[1]
								mqttGauge.WithLabelValues(topic).Set(value)
								log.Infof("MQTT TOPIC %s => %f", topic, value)
							},
						},
					},
				})
				if err != nil {
					log.Fatal(err)
				}
			}

			disconnected := <-errChan
			log.Infof("MQTT Client disconnected %v", disconnected)

			cli.Terminate()

			time.Sleep(1 * time.Second)
		}
	}()

	go func() {
		// Cleanup
		for {
			time.Sleep(10 * time.Second)
			now := time.Now()
			topicLastHandledMutex.Lock()
			for topic, last := range topicLastHandled {
				duration := now.Sub(last)
				if duration > retainTime {
					mqttGauge.DeleteLabelValues(topic)
					delete(topicLastHandled, topic)
					log.Infof("Deleted old topic %s", topic)
				}
			}
			topicLastHandledMutex.Unlock()
		}
	}()

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!DOCTYPE html>
		<title>MQTT Exporter</title>
		<h1>MQTT Exporter</h1>
		<p><a href="` + *metricsPath + `">Metrics</a>
		`))
	})

	log.Infoln("Listening on", *listenAddress)
	err = http.ListenAndServe(*listenAddress, nil)
	if err != nil {
		log.Fatal(err)
	}
}
