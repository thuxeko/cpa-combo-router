package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// heartbeatHost is a modelHost that never produces a first byte and records
// every frame the plugin emits. It is deliberately separate from fakeModelHost
// because the heartbeat runs on its own goroutine and needs its own lock.
type heartbeatHost struct {
	mu      sync.Mutex
	emitted [][]byte
	streams []string
}

func (host *heartbeatHost) Execute(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	return pluginapi.HostModelExecutionResponse{}, nil
}

func (host *heartbeatHost) StartStream(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	return pluginapi.HostModelStreamResponse{StreamID: "upstream-1"}, nil
}

// ReadStream blocks well past the attempt timeout: this models a target that
// accepted the request and then went quiet, which is the dead-air case.
func (host *heartbeatHost) ReadStream(string) (pluginapi.HostModelStreamReadResponse, error) {
	time.Sleep(3 * time.Second)
	return pluginapi.HostModelStreamReadResponse{Done: true}, nil
}

func (host *heartbeatHost) CloseStream(string) error { return nil }

func (host *heartbeatHost) Emit(streamID string, payload []byte) error {
	host.mu.Lock()
	defer host.mu.Unlock()
	host.streams = append(host.streams, streamID)
	host.emitted = append(host.emitted, append([]byte(nil), payload...))
	return nil
}

func (host *heartbeatHost) ClosePluginStream(string, string) {}

func (host *heartbeatHost) count() int {
	host.mu.Lock()
	defer host.mu.Unlock()
	return len(host.emitted)
}

func (host *heartbeatHost) frames() [][]byte {
	host.mu.Lock()
	defer host.mu.Unlock()
	return append([][]byte(nil), host.emitted...)
}

func (host *heartbeatHost) streamIDs() []string {
	host.mu.Lock()
	defer host.mu.Unlock()
	return append([]string(nil), host.streams...)
}

// TestKeepAlivePayloadMatchesProtocol pins the frame shape per response
// protocol. Getting this wrong is silent: CPA would deliver the bytes verbatim
// and a malformed frame would surface as a client-side parse error.
func TestKeepAlivePayloadMatchesProtocol(t *testing.T) {
	cases := []struct {
		protocol string
		want     string
	}{
		{"claude", ": keep-alive\n\n"},
		{"anthropic", ": keep-alive\n\n"},
		{"gemini", `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]}}]}`},
		{"openai", `{"choices":[{"delta":{},"index":0}]}`},
		{"openai-response", `{"choices":[{"delta":{},"index":0}]}`},
		{"", `{"choices":[{"delta":{},"index":0}]}`},
	}
	for _, testCase := range cases {
		got := string(keepAlivePayload(testCase.protocol))
		if got != testCase.want {
			t.Errorf("keepAlivePayload(%q) = %q, want %q", testCase.protocol, got, testCase.want)
		}
	}
}

// TestKeepAlivePayloadIsValidJSONForJSONProtocols guards the OpenAI/Gemini
// frames against a typo that would only show up on the wire.
func TestKeepAlivePayloadIsValidJSONForJSONProtocols(t *testing.T) {
	for _, protocol := range []string{"openai", "openai-response", "gemini"} {
		payload := keepAlivePayload(protocol)
		if !json.Valid(payload) {
			t.Errorf("keepAlivePayload(%q) = %q is not valid JSON", protocol, payload)
		}
	}
}

// TestStreamHeartbeatEmitsUntilStopped is the core contract: frames go out on
// the cadence while the attempt is waiting, and stop when the attempt ends.
func TestStreamHeartbeatEmitsUntilStopped(t *testing.T) {
	previous := streamHeartbeatInterval
	streamHeartbeatInterval = 5 * time.Millisecond
	defer func() { streamHeartbeatInterval = previous }()

	host := &heartbeatHost{}
	heartbeat := startStreamHeartbeat(host, "stream-1", "openai")
	if heartbeat == nil {
		t.Fatal("startStreamHeartbeat() = nil, want a running heartbeat")
	}

	deadline := time.Now().Add(2 * time.Second)
	for host.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	emitted := host.frames()
	if len(emitted) < 2 {
		t.Fatalf("emitted %d frames, want at least 2 before stopping", len(emitted))
	}
	for _, frame := range emitted {
		if string(frame) != `{"choices":[{"delta":{},"index":0}]}` {
			t.Fatalf("frame = %q, want the OpenAI no-op delta", frame)
		}
	}

	heartbeat.Stop()
	time.Sleep(30 * time.Millisecond)
	afterStop := host.count()

	time.Sleep(40 * time.Millisecond)
	if final := host.count(); final != afterStop {
		t.Fatalf("heartbeat emitted %d more frames after Stop()", final-afterStop)
	}
}

// TestStreamHeartbeatStopIsIdempotentAndNilSafe keeps every defer path honest:
// Stop() runs on nil receivers and after an explicit stop.
func TestStreamHeartbeatStopIsIdempotentAndNilSafe(t *testing.T) {
	var nilHeartbeat *streamHeartbeat
	nilHeartbeat.Stop()

	host := &heartbeatHost{}
	heartbeat := startStreamHeartbeat(host, "stream-1", "claude")
	if heartbeat == nil {
		t.Fatal("startStreamHeartbeat() = nil, want a running heartbeat")
	}
	heartbeat.Stop()
	heartbeat.Stop()
}

// TestStreamHeartbeatSkipsWhenUnusable means an attempt without a stream id or
// host never spawns a goroutine, so callers can defer Stop() unconditionally.
func TestStreamHeartbeatSkipsWhenUnusable(t *testing.T) {
	if heartbeat := startStreamHeartbeat(&heartbeatHost{}, "", "openai"); heartbeat != nil {
		t.Error("startStreamHeartbeat() with an empty stream id = non-nil, want nil")
	}
	if heartbeat := startStreamHeartbeat(nil, "stream-1", "openai"); heartbeat != nil {
		t.Error("startStreamHeartbeat() with a nil host = non-nil, want nil")
	}
}

// TestForwardStreamAttemptHeartbeatsWhileWaitingForFirstByte is the regression
// test for the dead-air bug: the target never produces a first byte, and the
// attempt must still emit frames so CPA commits the headers instead of leaving
// the client with nothing until its budget expires.
//
// The heartbeat now belongs to the caller (executeStreamWithHost), which owns it
// across retries and the pauses between them. This test drives that ownership
// directly: it starts the heartbeat the same way the caller does, runs the
// attempt, and asserts frames were emitted during the silent wait.
func TestForwardStreamAttemptHeartbeatsWhileWaitingForFirstByte(t *testing.T) {
	previous := streamHeartbeatInterval
	streamHeartbeatInterval = 5 * time.Millisecond
	defer func() { streamHeartbeatInterval = previous }()

	host := &heartbeatHost{}

	// Same call the streaming path makes: one heartbeat for the whole request.
	heartbeat := startStreamHeartbeat(host, "plugin-stream-1", "openai")
	defer heartbeat.Stop()

	done := make(chan streamAttemptOutcome, 1)
	go func() {
		done <- forwardStreamAttempt(
			pluginapi.ExecutorRequest{Model: "strong"},
			executionBody{body: []byte(`{"model":"strong"}`), entryProtocol: "openai", responseProtocol: "openai"},
			"ca2/qwen3.8-max-0902", "strong", "plugin-stream-1", host,
			300*time.Millisecond, routerConfig{}, modelRoute{Alias: "strong"}, "ca2/qwen3.8-max-0902",
			newRouteRuntime(time.Now),
		)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for host.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	if count := host.count(); count < 2 {
		t.Fatalf("attempt emitted %d heartbeat frames while waiting for the first byte, want at least 2", count)
	}

	<-done

	for _, frame := range host.frames() {
		if !strings.Contains(string(frame), `"delta"`) {
			t.Fatalf("emitted frame %q is not an OpenAI no-op delta", frame)
		}
	}
	for _, streamID := range host.streamIDs() {
		if streamID != "plugin-stream-1" {
			t.Fatalf("heartbeat emitted on stream %q, want the plugin stream id", streamID)
		}
	}
}
