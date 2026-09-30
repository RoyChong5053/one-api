package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Laisky/errors/v2"
	"github.com/gin-gonic/gin"

	"github.com/Laisky/one-api/common/config"
	metalib "github.com/Laisky/one-api/relay/meta"
	relaymodel "github.com/Laisky/one-api/relay/model"
)

// streamCutCode is the machine-readable error code/type emitted when a buffered
// upstream stream ends without a terminal finish_reason (a provider-side cut).
// Downstream relays use it to distinguish this failure from a genuine upstream
// HTTP error.
const streamCutCode = "upstream_cut"

// streamBufferWriter buffers an SSE response body in memory so the relay can
// decide, after the upstream stream ends, whether the response is complete
// (commit) or was cut by the provider (discard and retry another channel).
//
// It deliberately suppresses Flush so nothing reaches the client before that
// decision. When the buffer exceeds config.BufferedStreamMaxBytes, or the
// configured max wait elapses, it degrades to a pass-through writer (committing
// whatever it has) so a very large or very slow stream is never held back
// indefinitely. Once committed, the response can no longer be retried.
type streamBufferWriter struct {
	gin.ResponseWriter

	mu        sync.Mutex
	buf       bytes.Buffer
	status    int
	committed bool
	discarded bool

	maxBytes int
	timer    *time.Timer
}

// newStreamBufferWriter wraps w with an in-memory buffer. maxWait <= 0 disables
// the time-based force-commit.
func newStreamBufferWriter(w gin.ResponseWriter, maxBytes int, maxWait time.Duration) *streamBufferWriter {
	sbw := &streamBufferWriter{ResponseWriter: w, maxBytes: maxBytes}
	if maxWait > 0 {
		sbw.timer = time.AfterFunc(maxWait, sbw.ForceCommit)
	}
	return sbw
}

// Write buffers data until the buffer is committed. Over-cap writes degrade to
// pass-through by committing what has been buffered so far.
func (w *streamBufferWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed {
		return w.ResponseWriter.Write(data)
	}
	if w.discarded {
		return len(data), nil
	}

	n, _ := w.buf.Write(data)
	if w.maxBytes > 0 && w.buf.Len() > w.maxBytes {
		w.flushLocked()
	}
	return n, nil
}

// WriteString buffers s; see Write.
func (w *streamBufferWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// WriteHeader records the status code without committing it.
func (w *streamBufferWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}

// WriteHeaderNow is a no-op until the buffer is committed, so callers such as
// gin.Render cannot prematurely flush headers.
func (w *streamBufferWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		w.ResponseWriter.WriteHeaderNow()
	}
}

// Flush is suppressed until commit so SSE headers/body stay buffered.
func (w *streamBufferWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		w.ResponseWriter.Flush()
	}
}

// Status returns the recorded or underlying status code.
func (w *streamBufferWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != 0 {
		return w.status
	}
	return w.ResponseWriter.Status()
}

// Size returns the number of buffered bytes (or the underlying size once committed).
func (w *streamBufferWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return w.ResponseWriter.Size()
	}
	return w.buf.Len()
}

// Written reports whether anything has been committed to the real client.
func (w *streamBufferWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.committed || w.ResponseWriter.Written()
}

// Committed reports whether the buffer has already been flushed to the client.
func (w *streamBufferWriter) Committed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.committed
}

// Commit flushes the buffered response to the client and stops the timer.
func (w *streamBufferWriter) Commit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

// ForceCommit is the timer callback; it commits the buffer if it is still open.
func (w *streamBufferWriter) ForceCommit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.discarded {
		w.flushLocked()
	}
}

// Discard drops the buffered response without writing to the client. It is the
// mechanism that keeps a cut stream from reaching the client so the relay can
// retry the next channel safely. A response that was already committed (size or
// time cap) cannot be discarded.
func (w *streamBufferWriter) Discard() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return
	}
	w.discardLocked()
}

// Outcome parses the buffered SSE body and reports whether it looks complete.
// It must be called before Commit/Discard (the buffer is cleared afterwards).
func (w *streamBufferWriter) Outcome() streamBufferOutcome {
	w.mu.Lock()
	data := append([]byte(nil), w.buf.Bytes()...)
	w.mu.Unlock()
	return parseStreamOutcome(data)
}

// Settle makes the final commit/discard decision atomically, closing the race
// with the size/time-cap timer. It reports the parsed outcome, whether the
// response was discarded as a cut, and whether it had already been committed
// (by the cap/timeout) before settling. When upstreamFailed is true the buffer
// is discarded without inspection, since the caller already has an error.
func (w *streamBufferWriter) Settle(upstreamFailed bool) (outcome streamBufferOutcome, cut bool, committed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed {
		return streamBufferOutcome{}, false, true
	}
	if upstreamFailed {
		w.discardLocked()
		return streamBufferOutcome{}, false, false
	}

	outcome = parseStreamOutcome(w.buf.Bytes())
	if outcome.IsCut() {
		w.discardLocked()
		return outcome, true, false
	}
	w.flushLocked()
	return outcome, false, false
}

// discardLocked clears the buffer and cancels the timer. Caller holds w.mu.
func (w *streamBufferWriter) discardLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.discarded = true
	w.buf.Reset()
}

// flushLocked commits the buffer to the underlying writer. Caller holds w.mu.
func (w *streamBufferWriter) flushLocked() {
	if w.committed {
		return
	}
	w.committed = true
	if w.timer != nil {
		w.timer.Stop()
	}

	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(code)
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.buf.Reset()
	}
	w.ResponseWriter.Flush()
}

// streamBufferOutcome summarizes a buffered SSE body.
type streamBufferOutcome struct {
	// Chunks is the number of JSON data chunks parsed (excluding [DONE]).
	Chunks int
	// SawFinish is true once any choice carried a non-empty finish_reason.
	SawFinish bool
	// HasToolCall is true if any delta carried tool_calls.
	HasToolCall bool
	// UpstreamCut is true if the stream carried an explicit upstream_cut error
	// event (some adaptors, e.g. gemini, emit one on a provider-side cut).
	UpstreamCut bool
}

// IsCut reports whether the buffered stream should be treated as a provider-side
// cut: either an explicit upstream_cut event, or no terminal finish_reason at all.
func (o streamBufferOutcome) IsCut() bool {
	return o.UpstreamCut || !o.SawFinish
}

// streamChunkProbe is the minimal projection of an OpenAI-compatible SSE chunk
// used for cut detection.
type streamChunkProbe struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Type string `json:"type"`
		Code any    `json:"code"`
	} `json:"error"`
}

// parseStreamOutcome scans a buffered SSE body and derives cut signals. It only
// inspects "data:" lines and tolerates any non-JSON line (heartbeats, comments).
func parseStreamOutcome(buf []byte) streamBufferOutcome {
	var outcome streamBufferOutcome
	for _, rawLine := range bytes.Split(buf, []byte("\n")) {
		line := bytes.TrimSpace(rawLine)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		var probe streamChunkProbe
		if err := json.Unmarshal(payload, &probe); err != nil {
			continue
		}

		if probe.Error != nil {
			errType := strings.ToLower(strings.TrimSpace(probe.Error.Type))
			errCode := strings.ToLower(strings.TrimSpace(fmt.Sprint(probe.Error.Code)))
			if errType == streamCutCode || errCode == streamCutCode {
				outcome.UpstreamCut = true
			}
			continue
		}

		outcome.Chunks++
		for _, choice := range probe.Choices {
			if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
				outcome.SawFinish = true
			}
			if tc := bytes.TrimSpace(choice.Delta.ToolCalls); len(tc) > 0 && !bytes.Equal(tc, []byte("null")) {
				outcome.HasToolCall = true
			}
		}
	}
	return outcome
}

// shouldBufferStream reports whether the current streaming request targets a
// configured buffered-stream model. It matches both the requested (origin) and
// mapped (actual) model names so aliases such as "auto-agent" are covered.
func shouldBufferStream(c *gin.Context, meta *metalib.Meta) bool {
	if meta == nil || !meta.IsStream {
		return false
	}
	if len(config.BufferedStreamModels) == 0 {
		return false
	}
	if c.Writer.Written() {
		// Response already started; buffering cannot un-send it.
		return false
	}
	if config.IsBufferedStreamModel(meta.OriginModelName) {
		return true
	}
	return config.IsBufferedStreamModel(meta.ActualModelName)
}

// newStreamCutError builds the retryable error returned when a buffered stream
// was cut by the provider. It is an upstream 502 (not a one-api internal error)
// so the relay records a channel failure, suspends the ability (and, after
// enough consecutive cuts, auto-disables the channel) while retrying the next
// channel within the same request.
func newStreamCutError(meta *metalib.Meta) *relaymodel.ErrorWithStatusCode {
	modelName := ""
	if meta != nil {
		modelName = meta.ActualModelName
	}
	return &relaymodel.ErrorWithStatusCode{
		Error: relaymodel.Error{
			Message:  fmt.Sprintf("upstream stream ended without a terminal finish_reason (upstream_cut, model=%s)", modelName),
			Type:     relaymodel.ErrorTypeUpstream,
			Code:     streamCutCode,
			RawError: errors.New(streamCutCode),
		},
		StatusCode: http.StatusBadGateway,
	}
}
