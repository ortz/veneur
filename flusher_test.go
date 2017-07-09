package veneur

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	lightstep "github.com/lightstep/lightstep-tracer-go"
	"github.com/stretchr/testify/assert"
	"github.com/stripe/veneur/samplers"
)

func TestServerTags(t *testing.T) {
	metrics := []samplers.DDMetric{{
		Name:       "foo.bar.baz",
		Value:      [1][2]float64{{float64(time.Now().Unix()), float64(1.0)}},
		Tags:       []string{"gorch:frobble", "x:e"},
		MetricType: "rate",
		Interval:   10,
	}}

	finalizeMetrics("somehostname", []string{"a:b", "c:d"}, metrics)
	assert.Equal(t, "somehostname", metrics[0].Hostname, "Metric hostname uses argument")
	assert.Contains(t, metrics[0].Tags, "a:b", "Tags should contain server tags")
}

func TestHostPortExtract(t *testing.T) {
	h, p, _ := extractHostPort("https://github.com/stripe/veneur")

	assert.Equal(t, "github.com", h, "Host should contain extracted host")
	assert.Equal(t, "443", p, "Port should contain extracted port")
}

func TestHostMagicTag(t *testing.T) {
	metrics := []samplers.DDMetric{{
		Name:       "foo.bar.baz",
		Value:      [1][2]float64{{float64(time.Now().Unix()), float64(1.0)}},
		Tags:       []string{"gorch:frobble", "host:abc123", "x:e"},
		MetricType: "rate",
		Interval:   10,
	}}

	finalizeMetrics("badhostname", []string{"a:b", "c:d"}, metrics)
	assert.Equal(t, "abc123", metrics[0].Hostname, "Metric hostname should be from tag")
	assert.NotContains(t, metrics[0].Tags, "host:abc123", "Host tag should be removed")
	assert.Contains(t, metrics[0].Tags, "x:e", "Last tag is still around")
}

func TestDeviceMagicTag(t *testing.T) {
	metrics := []samplers.DDMetric{{
		Name:       "foo.bar.baz",
		Value:      [1][2]float64{{float64(time.Now().Unix()), float64(1.0)}},
		Tags:       []string{"gorch:frobble", "device:abc123", "x:e"},
		MetricType: "rate",
		Interval:   10,
	}}

	finalizeMetrics("badhostname", []string{"a:b", "c:d"}, metrics)
	assert.Equal(t, "abc123", metrics[0].DeviceName, "Metric devicename should be from tag")
	assert.NotContains(t, metrics[0].Tags, "device:abc123", "Host tag should be removed")
	assert.Contains(t, metrics[0].Tags, "x:e", "Last tag is still around")
}

func TestFlushTracesDatadog(t *testing.T) {
	type TestCase struct {
		Name         string
		ProtobufFile string
		JSONFile     string
	}

	cases := []TestCase{
		{
			Name:         "Success",
			ProtobufFile: filepath.Join("fixtures", "protobuf", "trace.pb"),
			JSONFile:     filepath.Join("fixtures", "tracing_agent", "spans", "trace.pb.json"),
		},
		{
			Name:         "Critical",
			ProtobufFile: filepath.Join("fixtures", "protobuf", "trace_critical.pb"),
			JSONFile:     filepath.Join("fixtures", "tracing_agent", "spans", "trace_critical.pb.json"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			pb, err := os.Open(tc.ProtobufFile)
			assert.NoError(t, err)
			defer pb.Close()

			js, err := os.Open(tc.JSONFile)
			assert.NoError(t, err)
			defer js.Close()

			testFlushTraceDatadog(t, pb, js)
		})
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%s", tc.Name, "lightstep"), func(t *testing.T) {
			pb, err := os.Open(tc.ProtobufFile)
			assert.NoError(t, err)
			defer pb.Close()

			js, err := os.Open(tc.JSONFile)
			assert.NoError(t, err)
			defer js.Close()

			testFlushTraceLightstep(t, pb, js)
		})
	}
}

func testFlushTraceDatadog(t *testing.T, protobuf, jsn io.Reader) {
	remoteResponseChan := make(chan struct{}, 1)
	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var expected []*DatadogTraceSpan
		err := json.NewDecoder(jsn).Decode(&expected)
		assert.NoError(t, err)

		var actual []*DatadogTraceSpan
		err = json.NewDecoder(r.Body).Decode(&actual)
		assert.NoError(t, err)

		assert.Equal(t, expected, actual)

		w.WriteHeader(http.StatusAccepted)

		remoteResponseChan <- struct{}{}
	}))
	defer remoteServer.Close()

	config := globalConfig()
	config.TraceAPIAddress = remoteServer.URL

	server := setupVeneurServer(t, config, nil)
	defer server.Shutdown()

	server.tracerSinks = append(server.tracerSinks, tracerSink{
		name:   "Datadog",
		tracer: nil,
		flush:  flushSpansDatadog,
	})

	assert.Equal(t, server.DDTraceAddress, config.TraceAPIAddress)

	packet, err := ioutil.ReadAll(protobuf)
	assert.NoError(t, err)

	server.HandleTracePacket(packet)

	assert.NoError(t, err)
	server.Flush()

	// wait for remoteServer to process the POST
	select {
	case <-remoteResponseChan:
		// all is safe
		break
	case <-time.After(10 * time.Second):
		assert.Fail(t, "Global server did not complete all responses before test terminated!")
	}
}

// testFlushTraceLightstep tests that the Lightstep sink can be initialized correctly
// and that the flushSpansLightstep function executes without error.
// We can't actually test the functionality end-to-end because the lightstep
// implementation doesn't expose itself for mocking.
func testFlushTraceLightstep(t *testing.T, protobuf, jsn io.Reader) {
	config := globalConfig()

	// this can be anything as long as it's not empty
	config.TraceAPIAddress = "http://example.org"
	server := setupVeneurServer(t, config, nil)
	defer server.Shutdown()

	lightstepTracer := lightstep.NewTracer(lightstep.Options{
		AccessToken: "TestAccessToken",
	})

	server.tracerSinks = append(server.tracerSinks, tracerSink{
		name:   "Lightstep",
		tracer: lightstepTracer,
		flush:  flushSpansLightstep,
	})

	assert.Equal(t, server.DDTraceAddress, config.TraceAPIAddress)

	packet, err := ioutil.ReadAll(protobuf)
	assert.NoError(t, err)

	server.HandleTracePacket(packet)

	assert.NoError(t, err)
	server.Flush()
}
