package ninja

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

	"git.eclipse.org/gitroot/paho/org.eclipse.paho.mqtt.golang.git"

	"github.com/ninjasphere/go-ninja/config"
	"github.com/ninjasphere/go-ninja/logger"
	"github.com/ninjasphere/go-ninja/model"
	"github.com/ninjasphere/go-ninja/rpc"
	"github.com/ninjasphere/go-ninja/rpc/json2"
)

// Connection Holds the connection to the Ninja MQTT bus, and provides all the methods needed to communicate with
// the other modules in Sphere.
type Connection struct {
	mqtt      *mqtt.MqttClient
	log       *logger.Logger
	rpc       *rpc.Client
	rpcServer *rpc.Server
}

type Driver interface {
	GetModuleInfo() *model.Module
	SetEventHandler(func(event string, payload interface{}) error)
}

type Device interface {
	GetDriver() Driver
	GetDeviceInfo() *model.Device
	SetEventHandler(func(event string, payload interface{}) error)
}

type Channel interface {
	GetProtocol() string
	SetEventHandler(func(event string, payload interface{}) error)
}

// Connect Builds a new ninja connection to the MQTT broker, using the given client ID
func Connect(clientID string) (*Connection, error) {

	log := logger.GetLogger("connection")

	conn := Connection{log: log}

	mqttURL := fmt.Sprintf("tcp://%s:%d", config.MustString("mqtt", "host"), config.MustInt("mqtt", "port"))

	opts := mqtt.NewClientOptions().AddBroker(mqttURL).SetClientId(clientID).SetCleanSession(true)
	conn.mqtt = mqtt.NewClient(opts)

	if _, err := conn.mqtt.Start(); err != nil {
		return nil, err
	}

	conn.rpc = rpc.NewClient(conn.mqtt, json2.NewClientCodec())
	conn.rpcServer = rpc.NewServer(conn.mqtt, json2.NewCodec())

	log.Infof("Connected to %s using cid:%s", mqttURL, clientID)

	job, err := CreateStatusJob(&conn, clientID)

	if err != nil {
		return nil, err
	}
	job.Start()

	return &conn, nil
}

// GetMqttClient will be removed in a later version. All communication should happen via methods on Connection
func (c *Connection) GetMqttClient() *mqtt.MqttClient {
	return c.mqtt
}

// Subscribe allows you to subscribe to an MQTT topic. Topics can contain variables of the form ":myvar" which will
// be returned in the values map in the callback. The callback must return "true" if it wants to receive more messages.
func (c *Connection) Subscribe(topic string, callback func(message mqtt.Message, values map[string]string) bool) error {

	filter, err := mqtt.NewTopicFilter(GetSubscribeTopic(topic), 0)
	if err != nil {
		c.log.FatalError(err, "Failed to subscribe to "+topic)
	}

	finished := false
	mutex := &sync.Mutex{}

	receipt, err := c.mqtt.StartSubscription(func(_ *mqtt.MqttClient, message mqtt.Message) {
		// We lock so that the callback has a chance to return false,
		// to prevent any more messages arriving on this subscription
		mutex.Lock()

		// TODO: Implement unsubscribing. For now, it will just skip over any subscriptions that have finished
		if finished {
			return
		}

		values, ok := MatchTopicPattern(topic, message.Topic())
		if !ok {
			c.log.Warningf("Failed to read params from topic: %s using template: %s", message.Topic(), topic)
			mutex.Unlock()
		} else {

			// The callback needs to be run in a goroutine as blocking this thread prevents any other messages arriving
			go func() {
				if !callback(message, *values) {
					// The callback has returned false, indicating that it does not want to receive any more messages.
					finished = true
				}
				mutex.Unlock()
			}()

		}
	}, filter)

	if err != nil {
		return err
	}

	<-receipt
	return nil
}

// GetServiceClient returns an RPC client for the given service.
func (c *Connection) GetServiceClient(serviceTopic string) *ServiceClient {
	return &ServiceClient{c, serviceTopic}
}

// ExportDriver Exports a driver using the 'driver' protocol, and announces it
func (c *Connection) ExportDriver(driver Driver) error {
	topic := fmt.Sprintf("$node/%s/driver/%s", config.Serial(), driver.GetModuleInfo().ID)

	announcement := driver.GetModuleInfo()

	announcement.ServiceAnnouncement = model.ServiceAnnouncement{
		Schema: "http://schema.ninjablocks.com/service/driver",
	}

	_, err := c.exportService(driver, topic, announcement)

	if err != nil {
		return err
	}

	return nil
}

// ExportDevice Exports a device using the 'device' protocol, and announces it
func (c *Connection) ExportDevice(device Device) error {
	announcement := device.GetDeviceInfo()
	announcement.GUID = getGUID(device.GetDeviceInfo().IDType, device.GetDeviceInfo().ID)

	topic := fmt.Sprintf("$device/%s", announcement.GUID)

	announcement.ServiceAnnouncement = model.ServiceAnnouncement{
		Schema: "http://schema.ninjablocks.com/service/device",
	}

	_, err := c.exportService(device, topic, announcement)

	if err != nil {
		return err
	}

	return nil
}

// ExportChannel Exports a device using the given protocol, and announces it
func (c *Connection) ExportChannel(device Device, channel Channel, id string) error {
	return c.ExportChannelWithSupported(device, channel, id, nil, nil)
}

// ExportChannelWithSupported is the same as ExportChannel, but any methods provided must actually be exported by the
// channel, or an error is returned
func (c *Connection) ExportChannelWithSupported(device Device, channel Channel, id string, supportedMethods *[]string, supportedEvents *[]string) error {
	if channel.GetProtocol() == "" {
		return fmt.Errorf("The channel must have a protocol. Channel ID: %s", id)
	}

	announcement := &model.Channel{
		ID:       id,
		Protocol: channel.GetProtocol(),
		Device:   device.GetDeviceInfo(),
	}

	topic := fmt.Sprintf("$device/%s/channel/%s", device.GetDeviceInfo().GUID, id)

	announcement.ServiceAnnouncement = model.ServiceAnnouncement{
		Schema:           resolveProtocolURI(channel.GetProtocol()),
		SupportedMethods: supportedMethods,
		SupportedEvents:  supportedEvents,
	}

	_, err := c.exportService(channel, topic, announcement)

	if err != nil {
		return err
	}

	return nil
}

type eventingService interface {
	SetEventHandler(func(event string, payload interface{}) error)
}

type serviceAnnouncement interface {
	GetServiceAnnouncement() *model.ServiceAnnouncement
}

// exportService Exports an RPC service, and announces it over TOPIC/event/announce
func (c *Connection) exportService(service interface{}, topic string, announcement serviceAnnouncement) (*rpc.ExportedService, error) {

	exportedService, err := c.rpcServer.RegisterService(service, topic)

	if err != nil {
		return nil, fmt.Errorf("Failed to register service on %s : %s", topic, err)
	}

	if announcement.GetServiceAnnouncement().SupportedMethods == nil {
		announcement.GetServiceAnnouncement().SupportedMethods = &exportedService.Methods
	} else {
		// TODO: Check that all strings in announcement.SupportedMethods exist in exportedService.Methods
		if len(*announcement.GetServiceAnnouncement().SupportedMethods) > len(exportedService.Methods) {
			return nil, fmt.Errorf("The number of actual exported methods is less than the number said to be exported. Check the method signatures of the service. topic:%s", topic)
		}
	}

	if announcement.GetServiceAnnouncement().SupportedEvents == nil {
		events := []string{}
		announcement.GetServiceAnnouncement().SupportedEvents = &events
	}

	// send out service announcement
	err = exportedService.SendEvent("announce", announcement)
	if err != nil {
		return nil, fmt.Errorf("Failed sending service announcement: %s", err)
	}

	c.log.Debugf("Exported service on topic: %s (schema: %s) with methods: %s", topic, announcement.GetServiceAnnouncement().Schema, strings.Join(*announcement.GetServiceAnnouncement().SupportedMethods, ", "))

	switch service := service.(type) {
	case eventingService:
		service.SetEventHandler(func(event string, payload interface{}) error {
			return exportedService.SendEvent(event, payload)
		})
	}

	return exportedService, nil
}

// SendNotification Sends a simple json-rpc notification to a topic
func (c *Connection) SendNotification(topic string, params ...interface{}) error {
	return c.rpcServer.SendNotification(topic, params...)
}

// Pull this out into the scham validation pakage when we have one
var rootSchemaURL, _ = url.Parse("http://schemas.ninjablocks.com")
var protocolSchemaURL, _ = url.Parse("http://schemas.ninjablocks.com/protocol/")

func resolveSchemaURI(uri string) string {
	return resolveSchemaURIWithBase(rootSchemaURL, uri)
}

func resolveProtocolURI(uri string) string {
	return resolveSchemaURIWithBase(protocolSchemaURL, uri)
}

func resolveSchemaURIWithBase(base *url.URL, uri string) string {

	u, err := url.Parse(uri)
	if err != nil {
		log.Fatalf("Expected URL to parse: %q, got error: %v", uri, err)
	}
	return rootSchemaURL.ResolveReference(u).String()
}
