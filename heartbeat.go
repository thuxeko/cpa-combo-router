package main

import (
	"sync"
	"time"
)

// heartbeatInterval is how often a keep-alive frame is emitted while the router
// is still waiting for a target's first byte.
//
// Why this exists: CPA holds the HTTP response — headers included — until the
// first upstream chunk arrives (see setSSEHeaders() in the OpenAI/Claude
// handlers, which only runs after the first chunk is received). Its own SSE
// keep-alive ticker lives in ForwardStream, which is entered AFTER that first
// chunk, so during the silent first-byte wait the client receives zero bytes and
// a total-request budget can expire. Measured on this box: a 700 KB request to
// ca2/qwen3.8-max-0902 has a TTFT of 32-40 s, while the calling app aborts at
// 60 s, and CPA records the abort as a 499.
//
// Emitting a no-op frame from the plugin forces CPA to commit the headers and
// start streaming, which is what keeps that budget from expiring.
//
// Cadence: 10 s, matching the peer router BSL-Router's tools.sse_keepalive
// default and CPA's own requests.streaming.keepalive-seconds (10). The earlier
// 5 s was twice as chatty as either; every tick costs a write + flush on a
// two-core host, so the thinner cadence is the deliberate choice. The client
// watchdog this must beat is 30 s of silence (Claude Code aborts with "Slow
// first byte: no stream chunk 30.0s after request sent"), so 10 s keeps a 3x
// margin without the extra chatter.
const heartbeatIntervalDefault = 10 * time.Second

// streamHeartbeatInterval is the live cadence. It is a variable so tests can
// drive it down instead of waiting seconds for a frame.
var streamHeartbeatInterval = heartbeatIntervalDefault

// keepAlivePayload builds the bytes an attempt should emit while it is still
// waiting for its first byte. CPA wraps an emitted chunk exactly the way it
// wraps an upstream payload chunk, so the bytes must already look like a stream
// frame of the response protocol:
//   - Claude handlers write the chunk verbatim, so a full SSE comment works and
//     is invisible to every client.
//   - Gemini handlers wrap it as "data: <chunk>\n\n".
//   - OpenAI chat/completions handlers wrap it as "data: <chunk>\n\n", so a
//     chunk with an empty delta is a legal no-op that clients skip.
func keepAlivePayload(responseProtocol string) []byte {
	switch normalizeProtocol(responseProtocol) {
	case "claude":
		return []byte(": keep-alive\n\n")
	case "gemini":
		return []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]}}]}`)
	default:
		return []byte(`{"choices":[{"delta":{},"index":0}]}`)
	}
}

// streamHeartbeat emits a protocol-appropriate no-op frame on a fixed cadence
// until it is stopped. Its only job is to get CPA to commit the response headers
// and start streaming before the target produces its first real byte.
type streamHeartbeat struct {
	stop chan struct{}
	once sync.Once
}

// startStreamHeartbeat begins emitting frames for the given response protocol.
// It returns nil when there is nothing to emit, so callers can Stop it blindly.
func startStreamHeartbeat(host modelHost, pluginStreamID, responseProtocol string) *streamHeartbeat {
	if host == nil || pluginStreamID == "" {
		return nil
	}
	payload := keepAlivePayload(responseProtocol)
	if len(payload) == 0 {
		return nil
	}
	heartbeat := &streamHeartbeat{stop: make(chan struct{})}
	// Read the cadence on the caller's goroutine. Reading it inside the
	// goroutine below would race with anything that sets it.
	interval := streamHeartbeatInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stop:
				return
			case <-ticker.C:
				// An emit failure means the client is already gone; there is
				// nothing left to keep alive.
				if err := host.Emit(pluginStreamID, payload); err != nil {
					return
				}
			}
		}
	}()
	return heartbeat
}

// Stop ends the heartbeat. It is safe to call more than once and on a nil
// receiver, so every exit path can defer it.
func (heartbeat *streamHeartbeat) Stop() {
	if heartbeat == nil {
		return
	}
	heartbeat.once.Do(func() { close(heartbeat.stop) })
}
