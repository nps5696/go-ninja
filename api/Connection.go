package ninja

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/nps5696/go-ninja/bus"
	"github.com/nps5696/go-ninja/config"
	"github.com/nps5696/go-ninja/logger"
	"github.com/nps5696/go-ninja/model"
	"github.com/nps5696/go-ninja/rpc"
	"github.com/nps5696/go-ninja/rpc/json2"
)

var (
	dummyRawCallback = func(params *json.RawMessage, values map[string]string) bool {
		return false
	}
)

// Connection Holds the connection to the Ninja MQTT bus, and provides all the methods needed to communicate with
// the other modules in Sphere.
type Connection struct {
	mqtt      bus.Bus
	log       *logger.Logger
	rpc       *rpc.Client
	rpcServer *rpc.Server
	services  []model.ServiceAnnouncement
}

// Connect Builds a new ninja connection to the MQTT broker, using the given client ID
func Connect(clientID string) (*Connection, error) {

	log := logger.GetLogger(fmt.Sprintf("%s.connection", clientID))

	conn := Connection{
		log:      log,
		services: []model.ServiceAnnouncement{},
	}

	mqttURL := fmt.Sprintf("%s:%d", config.MustString("mqtt", "host"), config.MustInt("mqtt", "port"))

	log.Infof("Connecting to %s using cid:%s", mqttURL, clientID)

	conn.mqtt = bus.MustConnect(mqttURL, clientID)

	log.Infof("Connected")

	conn.rpc = rpc.NewClient(conn.mqtt, json2.NewClientCodec())
	conn.rpcServer = rpc.NewServer(conn.mqtt, json2.NewCodec())

	// Add service discovery service. Responds to queries about services exposed in this process.
	discoveryService := &discoverService{&conn}
	_, err := conn.exportService(discoveryService, "$discover", &simpleService{*discoveryService.GetServiceAnnouncement()})
	if err != nil {
		log.Fatalf("Could not expose discovery service: %s", err)
	}

	return &conn, nil
}

// GetMqttClient will be removed in a later version. All communication should happen via methods on Connection
func (c *Connection) GetMqttClient() bus.Bus {
	return c.mqtt
}

type rpcMessage struct {
	Params *json.RawMessage `json:"params"`
}

// Subscribe allows you to subscribe to an MQTT topic. Topics can contain variables of the form ":myvar" which will
// be returned in the values map in the callback.
//
// The provided callback must be a function of 0, 1 or 2 parameters which returns
// "true" if it wants to receive more messages.
//
// The first parameter must either of type *json.RawMessage or else a pointer to a go struct type to which
// the expected event payload can be successfully unmarshalled.
//
// The second parameter should be of type map[string]string and will contain one value for each place holder
// specified in the topic string.
func (c *Connection) Subscribe(topic string, callback interface{}) (*bus.Subscription, error) {
	log.Println("Subscribing to " + topic)
	return c.subscribe(true, topic, callback)
}

func (c *Connection) SubscribeRaw(topic string, callback interface{}) (*bus.Subscription, error) {
	return c.subscribe(false, topic, callback)
}

func (c *Connection) subscribe(rpc bool, topic string, callback interface{}) (*bus.Subscription, error) {

	adapter, err := getAdapter(c.log, callback)
	if err != nil {
		c.log.FatalError(err, fmt.Sprintf("Incompatible callback function provided as callback for topic %s", topic))
		return nil, err
	}

	finished := false

	var sub *bus.Subscription
	sub, err = c.mqtt.Subscribe(GetSubscribeTopic(topic), func(incomingTopic string, payload []byte) {

		// TODO: Implement unsubscribing. For now, it will just skip over any subscriptions that have finished
		if finished {
			return
		}

		values, ok := MatchTopicPattern(topic, incomingTopic)
		if !ok {
			//c.log.Warningf("Failed to read params from topic: %s using template: %s", incomingTopic, topic)
			p := make(map[string]string)
			values = &p
		}

		var params json.RawMessage

		if rpc {
			msg := &rpcMessage{}
			err := json.Unmarshal(payload, msg)

			if err != nil {
				c.log.Warningf("Failed to read parameters in rpc call to %s - %v", incomingTopic, err)
				return
			}

			json2.ReadRPCParams(msg.Params, &params)
			if err != nil {
				c.log.Warningf("Failed to read parameters in rpc call to %s - %v", incomingTopic, err)
				return
			}
		} else {
			err := json.Unmarshal(payload, &params)

			if err != nil {
				c.log.Warningf("Failed to read parameters in call to %s - %v", incomingTopic, err)
				return
			}
		}

		if !adapter(&params, *values) {
			// The callback has returned false, indicating that it does not want to receive any more messages,
			// so we can cancel the subscription.
			sub.Cancel()
		}

	})

	return sub, err
}

// GetServiceClient returns an RPC client for the given service.
func (c *Connection) GetServiceClient(serviceTopic string) *ServiceClient {
	return &ServiceClient{
		conn:  c,
		Topic: serviceTopic,
	}
}

// GetServiceClientWithSupported returns an RPC client for the given service.
func (c *Connection) GetServiceClientFromAnnouncement(announcement model.ServiceAnnouncement) *ServiceClient {
	client := &ServiceClient{
		conn:  c,
		Topic: announcement.Topic,
	}

	if announcement.SupportedEvents != nil {
		client.SupportedEvents = *announcement.SupportedEvents
	}

	if announcement.SupportedMethods != nil {
		client.SupportedMethods = *announcement.SupportedMethods
	}

	return client
}

// ExportApp Exports an app using the 'app' protocol, and announces it
func (c *Connection) ExportApp(app App) error {

	if app.GetModuleInfo().ID == "" {
		panic("You must provide an ID in the package.json")
	}
	topic := fmt.Sprintf("$node/%s/app/%s", config.Serial(), app.GetModuleInfo().ID)

	announcement := app.GetModuleInfo()

	announcement.ServiceAnnouncement = model.ServiceAnnouncement{
		Schema: "http://schema.ninjablocks.com/service/app",
	}

	_, err := c.exportService(app, topic, announcement)

	if err != nil {
		return err
	}

	if config.Bool(false, "autostart") {
		err := c.GetServiceClient(topic).Call("start", struct{}{}, nil, time.Second*20)
		if err != nil {
			c.log.Fatalf("Failed to autostart app: %s", err)
		}
	}

	return nil
}

// ExportDriver Exports a driver using the 'driver' protocol, and announces it
func (c *Connection) ExportDriver(driver Driver) error {

	time.Sleep(config.Duration(time.Second*3, "drivers.startUpDelay"))

	topic := fmt.Sprintf("$node/%s/driver/%s", config.Serial(), driver.GetModuleInfo().ID)

	announcement := driver.GetModuleInfo()

	announcement.ServiceAnnouncement = model.ServiceAnnouncement{
		Schema: "http://schema.ninjablocks.com/service/driver",
	}

	_, err := c.exportService(driver, topic, announcement)

	if err != nil {
		return err
	}

	if config.Bool(false, "autostart") {
		err := c.GetServiceClient(topic).Call("start", struct{}{}, nil, time.Second*20)
		if err != nil {
			c.log.Fatalf("Failed to autostart driver: %s", err)
		}
	}

	return nil
}

// ExportDevice Exports a device using the 'device' protocol, and announces it
func (c *Connection) ExportDevice(device Device) error {
	announcement := device.GetDeviceInfo()
	announcement.ID = getGUID(device.GetDeviceInfo().NaturalIDType, device.GetDeviceInfo().NaturalID)

	topic := fmt.Sprintf("$device/%s", announcement.ID)

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
	}

	topic := fmt.Sprintf("$device/%s/channel/%s", device.GetDeviceInfo().ID, id)

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

func (c *Connection) ExportChannelWithModel(service interface{}, deviceTopic string, model *model.Channel) (*rpc.ExportedService, error) {
	return c.exportService(service, fmt.Sprintf("%s/channel/%s", deviceTopic, model.ID), model)
}

type simpleService struct {
	model.ServiceAnnouncement
}

func (s *simpleService) GetServiceAnnouncement() *model.ServiceAnnouncement {
	return &s.ServiceAnnouncement
}

// MustExportService Exports an RPC service, and announces it over TOPIC/event/announce. Must not cause an error or will panic.
func (c *Connection) MustExportService(service interface{}, topic string, announcement *model.ServiceAnnouncement) *rpc.ExportedService {
	exported, err := c.exportService(service, topic, &simpleService{*announcement})
	if err != nil {
		c.log.Fatalf("Failed to export service on topic '%s': %s", topic, err)
	}
	return exported
}

// ExportService Exports an RPC service, and announces it over TOPIC/event/announce
func (c *Connection) ExportService(service interface{}, topic string, announcement *model.ServiceAnnouncement) (*rpc.ExportedService, error) {
	return c.exportService(service, topic, &simpleService{*announcement})
}

type eventingServiceDeprecated interface {
	SetEventHandler(func(event string, payload interface{}) error)
}

type eventingService interface {
	SetEventHandler(func(event string, payload ...interface{}) error)
}

type serviceAnnouncement interface {
	GetServiceAnnouncement() *model.ServiceAnnouncement
}

// exportService Exports an RPC service, and announces it over TOPIC/event/announce
func (c *Connection) exportService(service interface{}, topic string, announcement serviceAnnouncement) (*rpc.ExportedService, error) {

	announcement.GetServiceAnnouncement().Schema = resolveSchemaURI(announcement.GetServiceAnnouncement().Schema)

	exportedService, err := c.rpcServer.RegisterService(service, topic, announcement.GetServiceAnnouncement().Schema)

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

	announcement.GetServiceAnnouncement().Topic = topic

	// send out service announcement
	err = exportedService.SendEvent("announce", announcement)
	if err != nil {
		return nil, fmt.Errorf("Failed sending service announcement: %s", err)
	}

	c.log.Debugf("Exported service on topic: %s (schema: %s) with methods: %s", topic, announcement.GetServiceAnnouncement().Schema, strings.Join(*announcement.GetServiceAnnouncement().SupportedMethods, ", "))

	switch service := service.(type) {
	case eventingServiceDeprecated:
		service.SetEventHandler(func(event string, payload interface{}) error {
			return exportedService.SendEvent(event, payload)
		})
	case eventingService:
		service.SetEventHandler(func(event string, payload ...interface{}) error {
			err := exportedService.SendEvent(event, payload...)
			if err != nil {
				c.log.Infof("Event failed to send: %s", err)
			}
			return err
		})
	}

	c.services = append(c.services, *announcement.GetServiceAnnouncement())

	return exportedService, nil
}

// PublishRaw sends a simple message
func (c *Connection) PublishRaw(topic string, payload ...interface{}) error {

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Failed to marshall mqtt message: %s", err)
	}

	c.mqtt.Publish(topic, jsonPayload)

	if err != nil {
		return fmt.Errorf("Failed to write publish message to MQTT: %s", err)
	}

	return nil
}

// PublishRawSingleValue sends a simple message with a single json payload
func (c *Connection) PublishRawSingleValue(topic string, payload interface{}) error {

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Failed to marshall mqtt message: %s", err)
	}

	c.mqtt.Publish(topic, jsonPayload)

	if err != nil {
		return fmt.Errorf("Failed to write publish message to MQTT: %s", err)
	}

	return nil
}

// SendNotification Sends a simple json-rpc notification to a topic
func (c *Connection) SendNotification(topic string, params ...interface{}) error {
	return c.rpcServer.SendNotification(topic, params...)
}

// Pull this out into the schema validation package when we have one
var rootSchemaURL, _ = url.Parse("http://schema.ninjablocks.com")
var protocolSchemaURL, _ = url.Parse("http://schema.ninjablocks.com/protocol/")

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
	return base.ResolveReference(u).String()
}

// support for reflective callbacks, modelled on approach used in rpc/server.go

type adapter struct {
	log      *logger.Logger
	function reflect.Value
	argCount int
	argType  reflect.Type
}

func (a *adapter) invoke(params *json.RawMessage, values map[string]string) bool {
	// self.log.Debugf("invoke: params=%s, values=%v", string(*params), values)
	var args []reflect.Value = make([]reflect.Value, a.argCount)

	switch a.argCount {
	case 2:
		args[1] = reflect.ValueOf(values)
		fallthrough
	case 1:
		arg := reflect.New(a.argType.Elem())
		err := json.Unmarshal(*params, arg.Interface())
		if err != nil {
			a.log.Errorf("failed to unmarshal %s as %v because %v", string(*params), arg, err)
			return true
		}
		args[0] = arg
	case 0:
	}
	return a.function.Call(args)[0].Interface().(bool)
}

func getAdapter(log *logger.Logger, callback interface{}) (func(params *json.RawMessage, values map[string]string) bool, error) {
	var err error = nil

	value := reflect.ValueOf(callback)
	valueType := value.Type()

	if valueType == reflect.ValueOf(dummyRawCallback).Type() {
		return callback.(func(params *json.RawMessage, values map[string]string) bool), nil
	}

	kind := value.Kind()
	if kind != reflect.Func {
		return nil, fmt.Errorf("%v is if kind %d, not of kind Func", callback, kind)
	}

	numIn := valueType.NumIn()

	var argType reflect.Type = nil
	empty := make(map[string]string)

	switch numIn {
	case 2:
		valuesType := valueType.In(1)
		if reflect.ValueOf(empty).Type() != valuesType {
			return nil, fmt.Errorf("second parameter, if specified must be of type map[string]string, is actually of type %v", valuesType)
		}
		fallthrough
	case 1:
		argType = valueType.In(0)
		argKind := argType.Kind()
		if argKind != reflect.Ptr {
			return nil, fmt.Errorf("type of first parameter %v must be of type Ptr, is actually of kind %d", argType, argKind)
		}
	case 0:
	default:
		return nil, fmt.Errorf("callback %v has too many (%d) parameters", callback, numIn)
	}

	numOut := valueType.NumOut()
	if numOut != 1 {
		return nil, fmt.Errorf("return type of %v has the wrong number (%d) of arguments", value, numOut)
	}

	if valueType.Out(0) != reflect.ValueOf(true).Type() {
		return nil, fmt.Errorf("return type of %v must be of type bool", value)
	}

	if err != nil {
		return nil, err
	}

	tmp := &adapter{
		log:      log,
		function: value,
		argCount: numIn,
		argType:  argType,
	}

	return tmp.invoke, err
}
