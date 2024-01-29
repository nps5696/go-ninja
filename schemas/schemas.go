package schemas

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/nps5696/go-ninja/config"
	"github.com/nps5696/go-ninja/logger"
	"github.com/nps5696/go-ninja/model"
	"github.com/ninjasphere/gojsonschema"
	"github.com/xeipuuv/gojsonreference"
)

var log = logger.GetLogger("schemas")

var root = "http://schema.ninjablocks.com/"
var rootURL, _ = url.Parse(root)
var filePrefix = config.MustString("installDirectory") + "/sphere-schemas/"
var fileSuffix = ".json"

var schemaPool = gojsonschema.NewSchemaPool()
var validationEnabled = config.Bool(false, "validate")

func init() {
	schemaPool.FilePrefix = &filePrefix
	schemaPool.FileSuffix = &fileSuffix

	if validationEnabled {
		log.Infof("-------- VALIDATION ENABLED --------")
	}
}

func Validate(schema string, obj interface{}) (*string, error) {

	if !validationEnabled {
		return nil, nil
	}

	jsonBytes, _ := json.Marshal(obj)
	var jsonPayload interface{}
	_ = json.Unmarshal(jsonBytes, &jsonPayload)

	log.Debugf("schema-validator: validating %s %s", schema, jsonBytes)

	doc, err := GetSchema(schema)

	if err != nil {
		return nil, fmt.Errorf("Failed to get document: %s", err)
	}

	// Try to validate the Json against the schema
	result := doc.Validate(jsonPayload)

	// Deal with result
	if !result.Valid() {
		messages := ""

		// Loop through errors
		for _, desc := range result.Errors() {
			messages += fmt.Sprintf("%s\n", desc)
		}
		return &messages, nil
	} else {
		return nil, nil
	}

}

func GetServiceMethods(service string) ([]string, error) {
	doc, err := GetDocument(service+"#/methods", true)

	if err != nil && fmt.Sprintf("%s", err) != "Object has no key 'methods'" {
		return nil, fmt.Errorf("Failed to load schema %s : %s", service, err)
	}

	methods := make([]string, 0, len(doc))
	for method := range doc {
		methods = append(methods, method)
	}

	return methods, nil
}

type flatItem struct {
	path  []string
	value interface{}
}

func flatten(input interface{}, lpath []string, flattened []flatItem) []flatItem {
	if lpath == nil {
		lpath = []string{}
	}
	if flattened == nil {
		flattened = []flatItem{}
	}

	if reflect.ValueOf(input).Kind() == reflect.Map {
		for rkey, value := range input.(map[string]interface{}) {
			flattened = flatten(value, append(lpath, rkey), flattened)
		}
	} else {
		flattened = append(flattened, flatItem{lpath, input})
	}

	return flattened
}

var timeSeriesPaths = make(map[string]string)

/*
* GetEventTimeSeriesData converts an event payload to 0..n time series data points.
* NOTE: The payload must already have been validated. No validation is done here.
* NOTE: This accepts the json payload. So either a simple type or map[string]interface{}
*
* @param value {interface{}} The payload of the event. Can be null if there is no payload
* @param eventSchemaUri {string} The URI of the schema defining the event (usually ends with #/events/{name})
* @returns {Array} An array of records that need to be saved to a time series db
 */
func GetEventTimeSeriesData(value interface{}, serviceSchemaUri, event string) ([]model.TimeSeriesDatapoint, error) {

	// We don't want a pointer, just grab the actual value
	if reflect.ValueOf(value).Kind() == reflect.Ptr {
		value = reflect.ValueOf(value).Elem().Interface()
	}

	var timeseriesData = make([]model.TimeSeriesDatapoint, 0)

	//log.Debugf("Finding time series data for service: %s event: %s from payload: %v", serviceSchemaUri, event, value)

	flat := flatten(value, nil, nil)

	for _, point := range flat {
		//log.Debugf("-- Checking: %v", point)

		refPath := "#/events/" + event + "/value"

		key := refPath
		if len(point.path) > 0 {
			key = strings.Join(append([]string{refPath}, point.path...), "/properties/")
		}

		extendedKey := serviceSchemaUri + key

		//log.Debugf("Created path %s", key)

		var timeseriesType string
		timeseriesType, ok := timeSeriesPaths[extendedKey]

		if !ok {

			pointSchema, err := GetDocument(serviceSchemaUri+refPath, true)

			for _, property := range point.path {

				props, ok := pointSchema["properties"].(map[string]interface{})
				if !ok {
					log.Warningf("Unknown property %s in service %s event %s. error: %s", property, serviceSchemaUri, event, err)
					ok = false
				}

				pointSchema, ok = props[property].(map[string]interface{})

				pointSchema, err = resolve(serviceSchemaUri+refPath, pointSchema)

				if !ok {
					log.Warningf("Unknown property %s in service %s event %s. error: %s", property, serviceSchemaUri, event, err)
					ok = false
				}

			}

			if err != nil {
				// As the data has been validated, this *shouldn't* happen. BUT we might be allowing unknown properties through.
				log.Warningf("Unknown property %s in service %s event %s. error: %s", refPath, serviceSchemaUri, event, err)
				ok = false
			} else {
				timeseriesType, ok = pointSchema["timeseries"].(string)
			}

			timeSeriesPaths[extendedKey] = timeseriesType
		}

		if ok && timeseriesType != "" {

			dp := model.TimeSeriesDatapoint{
				Path: strings.Join(point.path, "."),
				Type: timeseriesType,
			}

			switch timeseriesType {
			case "value", "boolean", "tag":
				dp.Value = point.value
			}

			// The only other type is 'event', which doesn't have or need a value

			timeseriesData = append(timeseriesData, dp)

		}
	}

	return timeseriesData, nil
}

func GetDocument(documentURL string, resolveRefs bool) (map[string]interface{}, error) {
	resolvedURL, err := resolveUrl(rootURL, documentURL)
	if err != nil {
		return nil, err
	}

	localURL := useLocalUrl(resolvedURL)

	doc, err := schemaPool.GetDocument(localURL)
	if err != nil {
		return nil, err
	}

	refURL, _ := url.Parse(documentURL)

	document := doc.Document

	if err == nil && refURL.Fragment != "" {
		// If we have a fragment, grab it.
		document, _, err = resolvedURL.GetPointer().Get(document)
	}

	if err != nil {
		return nil, err
	}

	mapDoc := document.(map[string]interface{})

	if resolveRefs {

		return resolve(documentURL, mapDoc)
		/*if ref, ok := mapDoc["$ref"]; ok && ref != "" {
			log.Debugf("Got $ref: %s", ref)
			var resolvedRef, err = resolveUrl(resolvedURL.GetUrl(), ref.(string))
			log.Debugf("resolved %s to %s", ref.(string), resolvedRef.GetUrl().String())
			if err != nil {
				return nil, err
			}
			return GetDocument(resolvedRef.String(), true)
		}*/
	}

	return mapDoc, nil
}

func resolve(documentURL string, doc map[string]interface{}) (map[string]interface{}, error) {

	if ref, ok := doc["$ref"]; ok && ref != "" {
		log.Debugf("Got $ref: %s", ref)

		resolvedURL, err := resolveUrl(rootURL, documentURL)
		if err != nil {
			return nil, err
		}

		resolvedRef, err := resolveUrl(resolvedURL.GetUrl(), ref.(string))
		log.Debugf("resolved %s to %s", ref.(string), resolvedRef.GetUrl().String())
		if err != nil {
			return nil, err
		}
		return GetDocument(resolvedRef.String(), true)
	}

	return doc, nil
}

type schemaResponse struct {
	schema *gojsonschema.JsonSchemaDocument
	err    error
}

var schemasCache = make(map[string]schemaResponse)

func GetSchema(documentURL string) (*gojsonschema.JsonSchemaDocument, error) {

	resolved, err := resolveUrl(rootURL, documentURL)
	if err != nil {
		return nil, err
	}
	localRef := useLocalUrl(resolved)
	local := localRef.GetUrl().String()

	schema, ok := schemasCache[local]
	if !ok {
		log.Debugf("Cache miss on '%s'", resolved.GetUrl().String())
		s, err := gojsonschema.NewJsonSchemaDocument(local, schemaPool)
		schema = schemaResponse{s, err}
		schemasCache[local] = schema
	}
	return schema.schema, schema.err
}

func useLocalUrl(ref gojsonreference.JsonReference) gojsonreference.JsonReference {
	// Grab ninjablocks schemas locally

	local := strings.Replace(ref.GetUrl().String(), root, "file:///", 1)
	log.Debugf("Fetching document from %s", local)
	localURL, _ := gojsonreference.NewJsonReference(local)
	return localURL
}

func resolveUrl(root *url.URL, documentURL string) (gojsonreference.JsonReference, error) {
	ref, err := gojsonreference.NewJsonReference(documentURL)
	if err != nil {
		return ref, err
	}
	resolvedURL := root.ResolveReference(ref.GetUrl())

	return gojsonreference.NewJsonReference(resolvedURL.String())
}

func main() {
	//spew.Dump(Validate("/protocol/humidity#/events/state/value", "hello"))
	//spew.Dump(Validate("protocol/humidity#/events/state/value", 10))

	// TODO: FAIL! min/max not taken care of!
	//spew.Dump(Validate("/protocol/humidity#/events/state/value", -10))

	//spew.Dump(GetServiceMethods("/protocol/power"))
	/*	doc, _ := GetDocument("/protocol/humidity", true)
		flattened := flatten(doc, []string{}, make([]flatItem, 0))
		spew.Dump(flattened)*/

	spew.Dump(GetEventTimeSeriesData(10, "/protocol/humidity", "state"))

	var payload = &testVal{
		Rumbling: true,
		X:        0.5,
		Y:        -0.1,
		Z: &testValSize{
			Hello:   10,
			Goodbye: 20,
		},
	}

	jsonBytes, _ := json.Marshal(payload)
	var jsonPayload interface{}
	_ = json.Unmarshal(jsonBytes, &jsonPayload)

	points, _ := GetEventTimeSeriesData(&jsonPayload, "/protocol/game-controller/joystick", "state")

	js, _ := json.Marshal(points)

	log.Infof("Points: %s", js)

	//spew.Dump(GetEventTimeSeriesData(nil, "/protocol/humidity", "state"))
}

type testVal struct {
	Rumbling bool         `json:"rumbling"`
	X        float64      `json:"x"`
	Y        float64      `json:"y"`
	Z        *testValSize `json:"z"`
}

type testValSize struct {
	Hello   int `json:"hello"`
	Goodbye int `json:"goodbye"`
}
