package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Laisky/one-api/common/config"
	metalib "github.com/Laisky/one-api/relay/meta"
)

func newTestBufferWriter(t *testing.T, maxBytes int) (*streamBufferWriter, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return newStreamBufferWriter(c.Writer, maxBytes, 0), rec
}

func TestParseStreamOutcome(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantCut    bool
		wantChunks int
		wantFinish bool
		wantTool   bool
		wantUpCut  bool
	}{
		{
			name: "complete stream with stop",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
			wantCut:    false,
			wantChunks: 2,
			wantFinish: true,
		},
		{
			name:       "empty stream is cut",
			body:       "data: [DONE]\n\n",
			wantCut:    true,
			wantChunks: 0,
		},
		{
			name:       "content but no terminal finish is cut",
			body:       "data: {\"choices\":[{\"delta\":{\"content\":\"par\"},\"finish_reason\":null}]}\n\n",
			wantCut:    true,
			wantChunks: 1,
		},
		{
			name: "tool_calls finish is complete",
			body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"a\"}]},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
			wantCut:    false,
			wantChunks: 2,
			wantFinish: true,
			wantTool:   true,
		},
		{
			name:      "explicit upstream_cut error event",
			body:      "data: {\"error\":{\"message\":\"cut\",\"type\":\"upstream_cut\",\"code\":\"upstream_cut\"}}\n\n",
			wantCut:   true,
			wantUpCut: true,
		},
		{
			name:       "non-json heartbeat lines ignored",
			body:       ": heartbeat\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
			wantCut:    false,
			wantChunks: 1,
			wantFinish: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oc := parseStreamOutcome([]byte(tt.body))
			require.Equal(t, tt.wantCut, oc.IsCut())
			require.Equal(t, tt.wantChunks, oc.Chunks)
			require.Equal(t, tt.wantFinish, oc.SawFinish)
			require.Equal(t, tt.wantTool, oc.HasToolCall)
			require.Equal(t, tt.wantUpCut, oc.UpstreamCut)
		})
	}
}

func TestStreamBufferWriterCommit(t *testing.T) {
	sbw, rec := newTestBufferWriter(t, 0)

	_, err := sbw.WriteString("data: hello\n\n")
	require.NoError(t, err)

	require.False(t, sbw.Committed())
	require.Empty(t, rec.Body.String(), "nothing must reach the client before commit")

	sbw.Commit()
	require.True(t, sbw.Committed())
	require.True(t, sbw.Written())
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "data: hello\n\n", rec.Body.String())
}

func TestStreamBufferWriterDiscard(t *testing.T) {
	sbw, rec := newTestBufferWriter(t, 0)

	_, err := sbw.WriteString("data: partial\n\n")
	require.NoError(t, err)
	sbw.Discard()

	require.False(t, sbw.Committed())
	require.Empty(t, rec.Body.String(), "discarded response must never reach the client")

	// Writes after discard are silently dropped and do not commit.
	_, err = sbw.WriteString("data: more\n\n")
	require.NoError(t, err)
	require.True(t, sbw.Outcome().IsCut(), "discarded buffer yields no finish_reason")
}

func TestStreamBufferWriterCapDegradesToPassthrough(t *testing.T) {
	sbw, rec := newTestBufferWriter(t, 4)

	_, err := sbw.WriteString("12345")
	require.NoError(t, err)

	require.True(t, sbw.Committed(), "exceeding the byte cap must force a commit")
	require.Equal(t, "12345", rec.Body.String())

	_, err = sbw.WriteString("67890")
	require.NoError(t, err)
	require.Equal(t, "1234567890", rec.Body.String())
}

func TestStreamBufferWriterSettle(t *testing.T) {
	// Complete stream commits.
	sbw, rec := newTestBufferWriter(t, 0)
	_, err := sbw.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	require.NoError(t, err)
	outcome, cut, committed := sbw.Settle(false)
	require.False(t, cut)
	require.False(t, committed)
	require.Equal(t, 1, outcome.Chunks)
	require.Equal(t, 200, rec.Code)
	require.NotEmpty(t, rec.Body.String())

	// Cut stream is discarded.
	sbw2, rec2 := newTestBufferWriter(t, 0)
	_, err = sbw2.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
	require.NoError(t, err)
	_, cut2, committed2 := sbw2.Settle(false)
	require.True(t, cut2)
	require.False(t, committed2)
	require.Empty(t, rec2.Body.String())

	// Upstream failure discards regardless of stream shape.
	sbw3, rec3 := newTestBufferWriter(t, 0)
	_, err = sbw3.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	require.NoError(t, err)
	_, cut3, committed3 := sbw3.Settle(true)
	require.False(t, cut3)
	require.False(t, committed3)
	require.Empty(t, rec3.Body.String())
}

func TestShouldBufferStream(t *testing.T) {
	orig := config.BufferedStreamModels
	t.Cleanup(func() { config.BufferedStreamModels = orig })

	gin.SetMode(gin.TestMode)
	newCtx := func() *gin.Context {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		return c
	}

	config.BufferedStreamModels = []string{"auto-agent"}

	require.True(t, shouldBufferStream(newCtx(), &metalib.Meta{IsStream: true, OriginModelName: "auto-agent"}))
	require.True(t, shouldBufferStream(newCtx(), &metalib.Meta{IsStream: true, ActualModelName: "AUTO-AGENT"}))
	require.False(t, shouldBufferStream(newCtx(), &metalib.Meta{IsStream: false, OriginModelName: "auto-agent"}))
	require.False(t, shouldBufferStream(newCtx(), &metalib.Meta{IsStream: true, OriginModelName: "auto-chat"}))

	config.BufferedStreamModels = nil
	require.False(t, shouldBufferStream(newCtx(), &metalib.Meta{IsStream: true, OriginModelName: "auto-agent"}))
}

func TestNewStreamCutErrorIsRetryableUpstream(t *testing.T) {
	errResp := newStreamCutError(&metalib.Meta{ActualModelName: "gemini-x"})
	require.Equal(t, 502, errResp.StatusCode)
	require.Equal(t, "upstream_cut", errResp.Code)
	require.Contains(t, strings.ToLower(errResp.Message), "upstream_cut")
}
