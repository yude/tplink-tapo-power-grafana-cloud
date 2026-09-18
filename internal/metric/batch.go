package metric

import (
	"math"
	"sort"
	"strconv"
	"time"
)

type Point struct {
	Name        string
	Unit        string
	Description string
	Value       float64
	Timestamp   time.Time
	Attributes  map[string]string
}

type Batch struct {
	points []Point
}

func (b *Batch) Add(point Point) {
	if point.Name == "" || math.IsNaN(point.Value) || math.IsInf(point.Value, 0) {
		return
	}
	if point.Unit == "" {
		point.Unit = "1"
	}
	if point.Timestamp.IsZero() {
		point.Timestamp = time.Now()
	}
	point.Attributes = cloneAttributes(point.Attributes)
	b.points = append(b.points, point)
}

func (b *Batch) Len() int { return len(b.points) }

func (b *Batch) Points() []Point {
	result := make([]Point, len(b.points))
	copy(result, b.points)
	return result
}

type OTLPRequest struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics"`
}

type ResourceMetrics struct {
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics"`
}

type Resource struct {
	Attributes []Attribute `json:"attributes"`
}

type ScopeMetrics struct {
	Scope   Scope        `json:"scope"`
	Metrics []OTLPMetric `json:"metrics"`
}

type Scope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type OTLPMetric struct {
	Name        string `json:"name"`
	Unit        string `json:"unit,omitempty"`
	Description string `json:"description,omitempty"`
	Gauge       Gauge  `json:"gauge"`
}

type Gauge struct {
	DataPoints []DataPoint `json:"dataPoints"`
}

type DataPoint struct {
	Attributes   []Attribute `json:"attributes,omitempty"`
	TimeUnixNano string      `json:"timeUnixNano"`
	AsDouble     float64     `json:"asDouble"`
}

type Attribute struct {
	Key   string         `json:"key"`
	Value AttributeValue `json:"value"`
}

type AttributeValue struct {
	StringValue string `json:"stringValue"`
}

func (b *Batch) OTLP(serviceName, version, instanceID string) OTLPRequest {
	type metricKey struct{ name, unit, description string }
	grouped := make(map[metricKey][]DataPoint)
	for _, point := range b.points {
		key := metricKey{point.Name, point.Unit, point.Description}
		grouped[key] = append(grouped[key], DataPoint{
			Attributes:   attributes(point.Attributes),
			TimeUnixNano: strconv.FormatInt(point.Timestamp.UnixNano(), 10),
			AsDouble:     point.Value,
		})
	}
	keys := make([]metricKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	metrics := make([]OTLPMetric, 0, len(keys))
	for _, key := range keys {
		metrics = append(metrics, OTLPMetric{
			Name: key.name, Unit: key.unit, Description: key.description,
			Gauge: Gauge{DataPoints: grouped[key]},
		})
	}
	return OTLPRequest{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: attributes(map[string]string{
			"service.name":        serviceName,
			"service.version":     version,
			"service.instance.id": instanceID,
		})},
		ScopeMetrics: []ScopeMetrics{{
			Scope: Scope{Name: serviceName, Version: version}, Metrics: metrics,
		}},
	}}}
}

func attributes(values map[string]string) []Attribute {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Attribute, 0, len(keys))
	for _, key := range keys {
		result = append(result, Attribute{Key: key, Value: AttributeValue{StringValue: values[key]}})
	}
	return result
}

func cloneAttributes(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
