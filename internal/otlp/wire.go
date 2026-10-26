package otlp

import "encoding/json"

// Wire types: the OTLP/JSON encoding of the protobuf messages in
// opentelemetry-proto (collector/logs/v1 ExportLogsServiceRequest,
// collector/metrics/v1 ExportMetricsServiceRequest and the logs/v1,
// metrics/v1, common/v1 and resource/v1 messages they contain). Field names
// are the proto3 JSON lowerCamelCase names; 64-bit integers (fixed64
// timestamps, int64 asInt/intValue) are decimal strings; enums are integers.
// Only the fields davit sets are declared.

// anyValue is common.v1.AnyValue; exactly one field is set.
type anyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// keyValue is common.v1.KeyValue.
type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// resource is resource.v1.Resource.
type resource struct {
	Attributes []keyValue `json:"attributes"`
}

// scope is common.v1.InstrumentationScope.
type scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// logRecord is logs.v1.LogRecord.
type logRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber"`
	SeverityText         string     `json:"severityText"`
	Body                 anyValue   `json:"body"`
	Attributes           []keyValue `json:"attributes,omitempty"`
}

type scopeLogs struct {
	Scope      scope       `json:"scope"`
	LogRecords []logRecord `json:"logRecords"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

// logsRequest is collector.logs.v1.ExportLogsServiceRequest.
type logsRequest struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

// numberDataPoint is metrics.v1.NumberDataPoint; one of AsDouble/AsInt.
type numberDataPoint struct {
	Attributes        []keyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	AsDouble          *float64   `json:"asDouble,omitempty"`
	AsInt             *string    `json:"asInt,omitempty"`
}

type gauge struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type sum struct {
	DataPoints             []numberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

// metric is metrics.v1.Metric; one of Gauge/Sum.
type metric struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Gauge       *gauge `json:"gauge,omitempty"`
	Sum         *sum   `json:"sum,omitempty"`
}

type scopeMetrics struct {
	Scope   scope    `json:"scope"`
	Metrics []metric `json:"metrics"`
}

type resourceMetrics struct {
	Resource     resource       `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

// metricsRequest is collector.metrics.v1.ExportMetricsServiceRequest.
type metricsRequest struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

// exportResponse covers ExportLogsServiceResponse and
// ExportMetricsServiceResponse (partial success). json.Number accepts the
// int64-as-string form and a bare number.
type exportResponse struct {
	PartialSuccess struct {
		RejectedLogRecords json.Number `json:"rejectedLogRecords"`
		RejectedDataPoints json.Number `json:"rejectedDataPoints"`
		ErrorMessage       string      `json:"errorMessage"`
	} `json:"partialSuccess"`
}

// Enum values (integers in OTLP/JSON).
const (
	severityInfo          = 9 // logs.v1.SeverityNumber SEVERITY_NUMBER_INFO
	temporalityCumulative = 2 // metrics.v1.AggregationTemporality CUMULATIVE
)
