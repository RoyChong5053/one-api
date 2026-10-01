package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Laisky/errors/v2"
	gmw "github.com/Laisky/gin-middlewares/v7"
	"github.com/Laisky/zap"
	"github.com/gin-gonic/gin"

	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/ctxkey"
	"github.com/Laisky/one-api/common/helper"
	"github.com/Laisky/one-api/common/logger"
	"github.com/Laisky/one-api/common/message"
	"github.com/Laisky/one-api/middleware"
	"github.com/Laisky/one-api/model"
	"github.com/Laisky/one-api/monitor"
	"github.com/Laisky/one-api/relay"
	"github.com/Laisky/one-api/relay/adaptor"
	"github.com/Laisky/one-api/relay/adaptor/openai"

	"github.com/Laisky/one-api/relay/channeltype"
	"github.com/Laisky/one-api/relay/controller"
	"github.com/Laisky/one-api/relay/meta"
	relaymodel "github.com/Laisky/one-api/relay/model"
	"github.com/Laisky/one-api/relay/pricing"
	quotautil "github.com/Laisky/one-api/relay/quota"
	"github.com/Laisky/one-api/relay/relaymode"
)

func buildTestRequest(model string) *relaymodel.GeneralOpenAIRequest {
	if model == "" {
		model = "gpt-4o-mini"
	}
	testRequest := &relaymodel.GeneralOpenAIRequest{
		MaxTokens: config.TestMaxTokens,
		Model:     model,
	}
	testMessage := relaymodel.Message{
		Role:    "user",
		Content: config.TestPrompt,
	}
	testRequest.Messages = append(testRequest.Messages, testMessage)
	return testRequest
}

func buildRerankTestRequest(model string) *relaymodel.RerankRequest {
	if strings.TrimSpace(model) == "" {
		model = "reranker"
	}
	topN := 2
	return &relaymodel.RerankRequest{
		Model:     model,
		Query:     "hello world",
		Documents: []string{"hello world", "goodbye world"},
		TopN:      &topN,
	}
}

func buildEmbeddingTestRequest(model string) *relaymodel.GeneralOpenAIRequest {
	if strings.TrimSpace(model) == "" {
		model = "text-embedding-3-small"
	}
	return &relaymodel.GeneralOpenAIRequest{
		Model: model,
		Input: "hello world",
	}
}

func getChannelTestMode(channel *model.Channel) int {
	custom := channel.GetSupportedEndpoints()
	var names []string
	if len(custom) > 0 {
		names = custom
	} else {
		names = channeltype.DefaultEndpointNamesForChannelType(channel.Type)
	}
	hasChat := false
	hasRerank := false
	hasEmbed := false
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "chat_completions":
			hasChat = true
		case "rerank":
			hasRerank = true
		case "embeddings":
			hasEmbed = true
		}
	}
	if !hasChat {
		if hasRerank {
			return relaymode.Rerank
		}
		if hasEmbed {
			return relaymode.Embeddings
		}
	}
	return relaymode.ChatCompletions
}

func parseRerankTestResponse(resp string) (string, error) {
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
		Data []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"data"`
		Error   *relaymodel.Error `json:"error"`
		Message string            `json:"message"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return "", errors.Wrap(err, "unmarshal rerank response")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", errors.New(parsed.Error.Message)
	}
	if strings.TrimSpace(parsed.Message) != "" && len(parsed.Results) == 0 && len(parsed.Data) == 0 {
		// Some error envelopes use "message" without "error"
		// If results are empty and message is present, treat as error only when no results
		// Fall through to empty check below
	}
	count := len(parsed.Results)
	if count == 0 {
		count = len(parsed.Data)
	}
	if count == 0 {
		return "", errors.New("rerank response has no results")
	}
	return fmt.Sprintf("rerank ok (%d results)", count), nil
}

func parseEmbeddingTestResponse(resp string) (string, error) {
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
			Object    string    `json:"object"`
		} `json:"data"`
		Error   *relaymodel.Error `json:"error"`
		Message string            `json:"message"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return "", errors.Wrap(err, "unmarshal embedding response")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", errors.New(parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return "", errors.New("embedding response has no data")
	}
	dim := len(parsed.Data[0].Embedding)
	if dim == 0 {
		return fmt.Sprintf("embedding ok (%d vectors)", len(parsed.Data)), nil
	}
	return fmt.Sprintf("embedding ok (dim=%d, count=%d)", dim, len(parsed.Data)), nil
}

func parseTestResponse(resp string) (*openai.TextResponse, string, error) {
	var response openai.TextResponse
	if err := json.Unmarshal([]byte(resp), &response); err != nil {
		return nil, "", errors.Wrap(err, "unmarshal test response")
	}
	if len(response.Choices) == 0 {
		return nil, "", errors.New("response has no choices")
	}

	content := response.Choices[0].Content
	switch v := content.(type) {
	case string:
		return &response, v, nil
	case []interface{}:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "text" {
				if txt, ok := m["text"].(string); ok {
					parts = append(parts, txt)
				}
			}
		}
		return &response, strings.Join(parts, "\n"), nil
	default:
		return nil, "", errors.New("response content is not string or array")
	}
}

// calculateTestCost calculates the actual cost that would have been charged for a test request
// This is used for informational purposes to track the real cost of testing operations
func calculateTestCost(usage *relaymodel.Usage, meta *meta.Meta, request *relaymodel.GeneralOpenAIRequest) int64 {
	if usage == nil {
		return 0
	}

	// Get model ratio and completion ratio using three-layer pricing system
	pricingAdaptor := relay.GetAdaptor(meta.ChannelType)
	modelRatio := pricing.GetModelRatioWithThreeLayers(request.Model, nil, pricingAdaptor)
	completionRatio := pricing.GetCompletionRatioWithThreeLayers(request.Model, nil, pricingAdaptor)

	// Use the same group ratio as set in the context (typically 1.0 for tests)
	groupRatio := 1.0 // Default group ratio for tests
	computeResult := quotautil.Compute(quotautil.ComputeInput{
		Usage:                  usage,
		ModelName:              request.Model,
		ModelRatio:             modelRatio,
		GroupRatio:             groupRatio,
		ChannelCompletionRatio: map[string]float64{request.Model: completionRatio},
		PricingAdaptor:         pricingAdaptor,
	})

	return computeResult.TotalQuota
}

// testChannel runs a single non-streaming completion against a channel.
//
// probe is optional: when non-nil it is populated with the signals the channel
// health engine needs (time to first token, generation span, completion
// tokens). Passing nil keeps the cheaper legacy behaviour used by the manual
// test endpoints.
func testChannel(ctx context.Context, channel *model.Channel, request *relaymodel.GeneralOpenAIRequest, probe *model.ChannelObservation) (responseMessage string, err error, openaiErr *relaymodel.Error) {
	lg := gmw.GetLogger(ctx)
	startTime := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Body:   nil,
		Header: make(http.Header),
	}
	c.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	c.Set(ctxkey.Config, cfg)
	middleware.SetupContextForSelectedChannel(c, channel, "")
	meta := meta.GetByContext(c)
	apiType := channeltype.ToAPIType(channel.Type)
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return "", errors.Wrapf(nil, "invalid api type: %d, adaptor is nil", apiType), nil
	}

	adaptor.Init(meta)
	// -----------------------------
	// Resolve model: origin -> mapped -> provider-specific actual
	// -----------------------------
	requestedModel := strings.TrimSpace(request.Model)
	resolvedModel := requestedModel
	modelMap := channel.GetModelMapping()

	// initial context for debugging
	lg.Debug("channel test: initial model context",
		zap.Int("channel_id", channel.Id),
		zap.Int("channel_type", channel.Type),
		zap.String("base_url", channel.GetBaseURL()),
		zap.String("requested_model", requestedModel),
		zap.String("stored_models", channel.Models),
	)

	if resolvedModel == "" || !strings.Contains(channel.Models, resolvedModel) {
		modelNames := strings.Split(channel.Models, ",")
		if len(modelNames) > 0 {
			resolvedModel = strings.TrimSpace(modelNames[0])
		}
	}

	if modelMap != nil && modelMap[resolvedModel] != "" {
		resolvedModel = modelMap[resolvedModel]
	}

	// Provider-specific actual model resolution (e.g., AWS ARN)
	actualModel := resolvedModel
	if channel.Type == channeltype.AwsClaude {
		if arnMap := channel.GetInferenceProfileArnMap(); arnMap != nil {
			if arn, ok := arnMap[resolvedModel]; ok && arn != "" {
				actualModel = arn
			}
		}
	}

	// Ensure meta carries both origin and actual model for downstream URL building
	meta.OriginModelName = requestedModel
	request.Model = resolvedModel
	meta.ActualModelName = actualModel
	// Also reflect the chosen model in context for any code that reads it later
	c.Set(ctxkey.RequestModel, resolvedModel)

	lg.Debug("channel test: resolved model context",
		zap.String("origin_model", meta.OriginModelName),
		zap.String("resolved_model", resolvedModel),
		zap.String("actual_model", meta.ActualModelName),
		zap.Int("api_type", apiType),
		zap.String("request_path", c.Request.URL.Path),
	)
	convertedRequest, err := adaptor.ConvertRequest(c, relaymode.ChatCompletions, request)
	if err != nil {
		return "", errors.Wrap(err, "failed to convert request"), nil
	}
	c.Set(ctxkey.ConvertedRequest, convertedRequest)

	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal converted request"), nil
	}

	// Capture usage information for accurate test logging
	var actualUsage *relaymodel.Usage
	defer func() {
		logContent := fmt.Sprintf("test channel %s succeed，response: %s", channel.Name, responseMessage)
		if err != nil || openaiErr != nil {
			errorMessage := ""
			if err != nil {
				errorMessage = err.Error()
			} else {
				errorMessage = openaiErr.Message
			}
			logContent = fmt.Sprintf("test channel %s failed, error: %s", channel.Name, errorMessage)
		}

		// Create test log with actual usage information if available
		testLog := &model.Log{
			ChannelId:       channel.Id,
			ModelName:       resolvedModel,
			OriginModelName: requestedModel,
			Content:         logContent,
			ElapsedTime:     helper.CalcElapsedTime(startTime),
		}

		// Include actual token usage and calculated cost in test logs for accurate cost tracking
		if actualUsage != nil {
			testLog.PromptTokens = actualUsage.PromptTokens
			testLog.CompletionTokens = actualUsage.CompletionTokens

			// Calculate the actual cost that would have been charged (for informational purposes)
			// This helps with cost tracking and budgeting while keeping tests free for users
			actualCost := calculateTestCost(actualUsage, meta, request)
			testLog.Quota = int(actualCost)
		}

		// Record test log once (DB), avoid duplicate console error logs
		go model.RecordTestLog(ctx, testLog)
	}()

	// Pre-build and log the upstream URL for debugging consistency
	if fullURL, urlErr := adaptor.GetRequestURL(meta); urlErr == nil {
		lg.Debug("prepare test request",
			zap.String("actual_model", meta.ActualModelName),
			zap.Int("channel_id", channel.Id),
			zap.Int("channel_type", channel.Type),
			zap.String("upstream_url", fullURL),
			zap.ByteString("test_request", jsonData))
	} else {
		// Return early if URL cannot be built (e.g., missing deployment for Azure)
		return "", errors.Wrap(urlErr, "failed to build upstream request URL"), nil
	}
	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(requestBody)
	var resp *http.Response
	resp, err = adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		// Return wrapped error; avoid duplicate logging here
		return "", errors.Wrap(err, "failed to do request"), nil
	}

	// Handle nil response (e.g., AWS Bedrock uses SDK directly, not HTTP)
	if resp != nil {
		defer resp.Body.Close()
	}

	if resp != nil && resp.StatusCode != http.StatusOK {
		// Use context-aware error handler to capture and log upstream error response
		wrappedErr := controller.RelayErrorHandlerWithContext(c, resp)
		errorMessage := wrappedErr.Error.Message
		if errorMessage != "" {
			errorMessage = ", error message: " + errorMessage
		}
		err = errors.Wrapf(nil, "http status code: [%d]%s", resp.StatusCode, errorMessage)
		// Return error; detailed body already logged by RelayErrorHandlerWithContext when debug enabled
		return "", err, &wrappedErr.Error
	}

	usage, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		err = errors.Wrapf(nil, "response error: %s", respErr.Error.Message)
		return "", err, &respErr.Error
	}
	if usage == nil {
		err = errors.New("usage is nil")
		return "", errors.WithStack(err), nil
	}

	// Capture usage for test logging
	actualUsage = usage
	if probe != nil {
		populateProbeMetrics(probe, c, startTime, usage)
	}
	rawResponse := w.Body.String()
	_, responseMessage, err = parseTestResponse(rawResponse)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse test response: %s", rawResponse), nil
	}

	result := w.Result()
	// print result.Body
	var respBody []byte
	respBody, err = io.ReadAll(result.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read result body"), nil
	}

	statusCode := responseStatus(resp)

	lg.Debug("testing channel response",
		zap.Int("channel_id", channel.Id),
		zap.Bool("response_nil", resp == nil),
		zap.Int("status", statusCode),
		zap.Int("response_bytes", len(respBody)))
	return responseMessage, nil, nil
}

func testRerankChannel(ctx context.Context, channel *model.Channel, request *relaymodel.RerankRequest) (responseMessage string, err error, openaiErr *relaymodel.Error) {
	lg := gmw.GetLogger(ctx)
	startTime := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: "/v1/rerank"},
		Body:   nil,
		Header: make(http.Header),
	}
	c.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	c.Set(ctxkey.Config, cfg)
	middleware.SetupContextForSelectedChannel(c, channel, "")
	meta := meta.GetByContext(c)
	apiType := channeltype.ToAPIType(channel.Type)
	adaptorImpl := relay.GetAdaptor(apiType)
	if adaptorImpl == nil {
		return "", errors.Wrapf(nil, "invalid api type: %d, adaptor is nil", apiType), nil
	}
	adaptorImpl.Init(meta)

	requestedModel := strings.TrimSpace(request.Model)
	resolvedModel := requestedModel
	modelMap := channel.GetModelMapping()
	if resolvedModel == "" || !strings.Contains(channel.Models, resolvedModel) {
		modelNames := strings.Split(channel.Models, ",")
		if len(modelNames) > 0 {
			resolvedModel = strings.TrimSpace(modelNames[0])
		}
	}
	if modelMap != nil && modelMap[resolvedModel] != "" {
		resolvedModel = modelMap[resolvedModel]
	}
	request.Model = resolvedModel
	meta.OriginModelName = requestedModel
	meta.ActualModelName = resolvedModel
	c.Set(ctxkey.RequestModel, resolvedModel)

	lg.Debug("channel test (rerank): resolved model",
		zap.String("origin_model", meta.OriginModelName),
		zap.String("resolved_model", resolvedModel),
		zap.Int("api_type", apiType),
		zap.String("request_path", c.Request.URL.Path),
	)

	rerankAdaptor, ok := adaptorImpl.(adaptor.RerankAdaptor)
	if !ok {
		return "", errors.Errorf("rerank not supported by adaptor type %d", channel.Type), nil
	}
	convertedRequest, convErr := rerankAdaptor.ConvertRerankRequest(c, request)
	if convErr != nil {
		return "", errors.Wrap(convErr, "failed to convert rerank request"), nil
	}
	c.Set(ctxkey.ConvertedRequest, convertedRequest)
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal rerank request"), nil
	}

	var actualUsage *relaymodel.Usage
	defer func() {
		logContent := fmt.Sprintf("test channel %s succeed，response: %s", channel.Name, responseMessage)
		if err != nil || openaiErr != nil {
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				msg = openaiErr.Message
			}
			logContent = fmt.Sprintf("test channel %s failed, error: %s", channel.Name, msg)
		}
		testLog := &model.Log{
			ChannelId:       channel.Id,
			ModelName:       resolvedModel,
			OriginModelName: requestedModel,
			Content:         logContent,
			ElapsedTime:     helper.CalcElapsedTime(startTime),
		}
		if actualUsage != nil {
			testLog.PromptTokens = actualUsage.PromptTokens
			testLog.CompletionTokens = actualUsage.CompletionTokens
		}
		go model.RecordTestLog(ctx, testLog)
	}()

	if fullURL, urlErr := adaptorImpl.GetRequestURL(meta); urlErr == nil {
		lg.Debug("prepare rerank test request",
			zap.String("actual_model", meta.ActualModelName),
			zap.Int("channel_id", channel.Id),
			zap.String("upstream_url", fullURL),
			zap.ByteString("test_request", jsonData))
	} else {
		return "", errors.Wrap(urlErr, "failed to build upstream request URL"), nil
	}
	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(requestBody)
	var resp *http.Response
	resp, err = adaptorImpl.DoRequest(c, meta, requestBody)
	if err != nil {
		return "", errors.Wrap(err, "failed to do request"), nil
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		wrappedErr := controller.RelayErrorHandlerWithContext(c, resp)
		msg := wrappedErr.Error.Message
		if msg != "" {
			msg = ", error message: " + msg
		}
		err = errors.Wrapf(nil, "http status code: [%d]%s", resp.StatusCode, msg)
		return "", err, &wrappedErr.Error
	}
	usage, respErr := adaptorImpl.DoResponse(c, resp, meta)
	if respErr != nil {
		err = errors.Wrapf(nil, "response error: %s", respErr.Error.Message)
		return "", err, &respErr.Error
	}
	// usage may be nil for rerank forwarding that writes directly; treat as non-fatal
	if usage != nil {
		actualUsage = usage
	}
	rawResponse := w.Body.String()
	msg, perr := parseRerankTestResponse(rawResponse)
	if perr != nil {
		return "", errors.Wrapf(perr, "failed to parse rerank response: %s", rawResponse), nil
	}
	responseMessage = msg
	result := w.Result()
	respBody, _ := io.ReadAll(result.Body)
	lg.Debug("testing rerank channel response",
		zap.Int("channel_id", channel.Id),
		zap.Int("status", responseStatus(resp)),
		zap.Int("response_bytes", len(respBody)))
	return responseMessage, nil, nil
}

func testEmbeddingChannel(ctx context.Context, channel *model.Channel, request *relaymodel.GeneralOpenAIRequest) (responseMessage string, err error, openaiErr *relaymodel.Error) {
	lg := gmw.GetLogger(ctx)
	startTime := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: "/v1/embeddings"},
		Body:   nil,
		Header: make(http.Header),
	}
	c.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	c.Set(ctxkey.Config, cfg)
	middleware.SetupContextForSelectedChannel(c, channel, "")
	meta := meta.GetByContext(c)
	apiType := channeltype.ToAPIType(channel.Type)
	adaptorImpl := relay.GetAdaptor(apiType)
	if adaptorImpl == nil {
		return "", errors.Wrapf(nil, "invalid api type: %d, adaptor is nil", apiType), nil
	}
	adaptorImpl.Init(meta)

	requestedModel := strings.TrimSpace(request.Model)
	resolvedModel := requestedModel
	modelMap := channel.GetModelMapping()
	if resolvedModel == "" || !strings.Contains(channel.Models, resolvedModel) {
		modelNames := strings.Split(channel.Models, ",")
		if len(modelNames) > 0 {
			resolvedModel = strings.TrimSpace(modelNames[0])
		}
	}
	if modelMap != nil && modelMap[resolvedModel] != "" {
		resolvedModel = modelMap[resolvedModel]
	}
	request.Model = resolvedModel
	meta.OriginModelName = requestedModel
	meta.ActualModelName = resolvedModel
	c.Set(ctxkey.RequestModel, resolvedModel)

	lg.Debug("channel test (embeddings): resolved model",
		zap.String("origin_model", meta.OriginModelName),
		zap.String("resolved_model", resolvedModel),
		zap.Int("api_type", apiType),
		zap.String("request_path", c.Request.URL.Path),
	)

	convertedRequest, err := adaptorImpl.ConvertRequest(c, relaymode.Embeddings, request)
	if err != nil {
		return "", errors.Wrap(err, "failed to convert embedding request"), nil
	}
	c.Set(ctxkey.ConvertedRequest, convertedRequest)
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal embedding request"), nil
	}

	var actualUsage *relaymodel.Usage
	defer func() {
		logContent := fmt.Sprintf("test channel %s succeed，response: %s", channel.Name, responseMessage)
		if err != nil || openaiErr != nil {
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				msg = openaiErr.Message
			}
			logContent = fmt.Sprintf("test channel %s failed, error: %s", channel.Name, msg)
		}
		testLog := &model.Log{
			ChannelId:       channel.Id,
			ModelName:       resolvedModel,
			OriginModelName: requestedModel,
			Content:         logContent,
			ElapsedTime:     helper.CalcElapsedTime(startTime),
		}
		if actualUsage != nil {
			testLog.PromptTokens = actualUsage.PromptTokens
			testLog.CompletionTokens = actualUsage.CompletionTokens
		}
		go model.RecordTestLog(ctx, testLog)
	}()

	if fullURL, urlErr := adaptorImpl.GetRequestURL(meta); urlErr == nil {
		lg.Debug("prepare embedding test request",
			zap.String("actual_model", meta.ActualModelName),
			zap.Int("channel_id", channel.Id),
			zap.String("upstream_url", fullURL),
			zap.ByteString("test_request", jsonData))
	} else {
		return "", errors.Wrap(urlErr, "failed to build upstream request URL"), nil
	}
	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(requestBody)
	var resp *http.Response
	resp, err = adaptorImpl.DoRequest(c, meta, requestBody)
	if err != nil {
		return "", errors.Wrap(err, "failed to do request"), nil
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		wrappedErr := controller.RelayErrorHandlerWithContext(c, resp)
		msg := wrappedErr.Error.Message
		if msg != "" {
			msg = ", error message: " + msg
		}
		err = errors.Wrapf(nil, "http status code: [%d]%s", resp.StatusCode, msg)
		return "", err, &wrappedErr.Error
	}
	usage, respErr := adaptorImpl.DoResponse(c, resp, meta)
	if respErr != nil {
		err = errors.Wrapf(nil, "response error: %s", respErr.Error.Message)
		return "", err, &respErr.Error
	}
	if usage != nil {
		actualUsage = usage
	}
	rawResponse := w.Body.String()
	msg, perr := parseEmbeddingTestResponse(rawResponse)
	if perr != nil {
		return "", errors.Wrapf(perr, "failed to parse embedding response: %s", rawResponse), nil
	}
	responseMessage = msg
	result := w.Result()
	respBody, _ := io.ReadAll(result.Body)
	lg.Debug("testing embedding channel response",
		zap.Int("channel_id", channel.Id),
		zap.Int("status", responseStatus(resp)),
		zap.Int("response_bytes", len(respBody)))
	return responseMessage, nil, nil
}

// responseStatus returns the response status code or zero when the response is nil.
func responseStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}

	return resp.StatusCode
}

// TestChannel executes a live request against the specified channel to verify availability.
func TestChannel(c *gin.Context) {
	lg := gmw.GetLogger(c).Named("test_channel")

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		lg.Debug("invalid channel id", zap.Error(err))
		helper.RespondError(c, err)
		return
	}

	lg = lg.With(zap.Int("channel_id", id))
	channel, err := model.GetChannelById(id, true)
	if err != nil {
		lg.Debug("failed to get channel by id", zap.Error(err))
		helper.RespondError(c, err)
		return
	}

	modelName := strings.TrimSpace(c.Query("model"))
	// If not explicitly provided by query, use stored testing_model; if missing, default to cheapest supported model
	if modelName == "" {
		if channel.TestingModel != nil && *channel.TestingModel != "" {
			// ensure still supported; if not, clear per requirement
			tm := *channel.TestingModel
			supported := slices.Contains(channel.GetSupportedModelNames(), tm)

			if supported {
				modelName = tm
			} else {
				// clear invalid stored value and pick cheapest
				channel.TestingModel = nil
				if err := model.DB.Model(channel).Where("id = ?", channel.Id).Update("testing_model", nil).Error; err != nil {
					lg.Error("failed to clear invalid testing_model", zap.Error(err))
				}
			}
		}

		if modelName == "" {
			modelName = channel.GetCheapestSupportedModel()
		}
	}

	ctx := gmw.SetLogger(c, lg)

	mode := getChannelTestMode(channel)
	tik := time.Now()
	var responseMessage string
	var openaiErr *relaymodel.Error
	switch mode {
	case relaymode.Rerank:
		rerankReq := buildRerankTestRequest(modelName)
		responseMessage, err, openaiErr = testRerankChannel(ctx, channel, rerankReq)
	case relaymode.Embeddings:
		embedReq := buildEmbeddingTestRequest(modelName)
		responseMessage, err, openaiErr = testEmbeddingChannel(ctx, channel, embedReq)
	default:
		testRequest := buildTestRequest(modelName)
		responseMessage, err, openaiErr = testChannel(ctx, channel, testRequest, nil)
	}
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	if err != nil || openaiErr != nil {
		milliseconds = 0
	}

	go channel.UpdateResponseTime(milliseconds)
	consumedTime := float64(milliseconds) / 1000.0
	if err != nil || openaiErr != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": func() string {
				if err != nil {
					return err.Error()
				}
				if openaiErr != nil {
					return openaiErr.Message
				}
				return ""
			}(),
			"time":      consumedTime,
			"modelName": modelName,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"message":   responseMessage,
		"time":      consumedTime,
		"modelName": modelName,
	})
}

var testAllChannelsLock sync.Mutex
var testAllChannelsRunning bool = false

func testChannels(ctx context.Context, notify bool, scope string) error {
	if config.RootUserEmail == "" {
		config.RootUserEmail = model.GetRootUserEmail()
	}
	testAllChannelsLock.Lock()
	if testAllChannelsRunning {
		testAllChannelsLock.Unlock()
		return errors.WithStack(errors.New("Test is already running"))
	}
	testAllChannelsRunning = true
	testAllChannelsLock.Unlock()
	channels, err := model.GetAllChannels(0, 0, scope, "", "")
	if err != nil {
		return errors.Wrap(err, "failed to get all channels")
	}
	go func() {
		lg := gmw.GetLogger(ctx)
		for _, channel := range channels {
			isChannelEnabled := channel.Status == model.ChannelStatusEnabled
			tik := time.Now()
			// Determine model for this channel: stored testing_model if valid, else cheapest
			chosenModel := ""
			if channel.TestingModel != nil && *channel.TestingModel != "" {
				tm := *channel.TestingModel
				valid := slices.Contains(channel.GetSupportedModelNames(), tm)
				if valid {
					chosenModel = tm
				} else {
					channel.TestingModel = nil
					if err := model.DB.Model(channel).Where("id = ?", channel.Id).Update("testing_model", nil).Error; err != nil {
						lg.Error("failed to clear invalid testing_model in bulk test", zap.Error(err))
					}
				}
			}
			if chosenModel == "" {
				chosenModel = channel.GetCheapestSupportedModel()
			}
			mode := getChannelTestMode(channel)
			var err error
			var openaiErr *relaymodel.Error
			switch mode {
			case relaymode.Rerank:
				rerankReq := buildRerankTestRequest(chosenModel)
				_, err, openaiErr = testRerankChannel(ctx, channel, rerankReq)
			case relaymode.Embeddings:
				embedReq := buildEmbeddingTestRequest(chosenModel)
				_, err, openaiErr = testEmbeddingChannel(ctx, channel, embedReq)
			default:
				testRequest := buildTestRequest(chosenModel)
				_, err, openaiErr = testChannel(ctx, channel, testRequest, nil)
			}
			tok := time.Now()
			milliseconds := tok.Sub(tik).Milliseconds()
			if isChannelEnabled && milliseconds >= config.ChannelHealthDisableLatencyMs {
				// Slow is not the same as wedged. Only a probe past the hang
				// threshold disables the channel; anything merely sluggish is
				// recorded as a slow observation so the health engine downranks
				// it and the router prefers a livelier peer. Previously any
				// probe over ChannelDisableThreshold took the channel out of
				// rotation entirely, which turned a mildly slow provider into
				// an outage.
				err = errors.Errorf("Response time %.2fs exceeds hang threshold %.2fs",
					float64(milliseconds)/1000.0, float64(config.ChannelHealthDisableLatencyMs)/1000.0)
				if config.AutomaticDisableChannelEnabled {
					monitor.DisableChannel(channel.Id, channel.Name, err.Error())
				} else {
					_ = message.Notify(message.ByAll, fmt.Sprintf("Channel %s （%d）Test超时", channel.Name, channel.Id), "", err.Error())
				}
			} else if isChannelEnabled && milliseconds >= int64(config.ChannelHealthLatencySlowMs) {
				lg.Debug("channel probe degraded by slow response time",
					zap.Int("channel_id", channel.Id),
					zap.Int64("latency_ms", milliseconds),
					zap.Float64("slow_threshold_ms", config.ChannelHealthLatencySlowMs),
				)
			}
			// Only disable a channel on failure when AutomaticDisableChannelEnabled is true.
			if isChannelEnabled && (err != nil || monitor.ShouldDisableChannel(openaiErr, -1)) {
				// Build a safe reason string to avoid nil dereference
				reason := "channel test failed"
				if err != nil {
					reason = err.Error()
				} else if openaiErr != nil {
					reason = openaiErr.Message
				}
				if config.AutomaticDisableChannelEnabled {
					monitor.DisableChannel(channel.Id, channel.Name, reason)
				} else {
					// Notify only when auto-disable is off
					_ = message.Notify(message.ByAll, fmt.Sprintf("Channel %s （%d）Test失败", channel.Name, channel.Id), "", reason)
				}
			}
			// Record the probe result into the health engine so the auto-disabled
			// sweep feeds the same score the router gates on. Without this the
			// recovery decision would be made on a single test result with no
			// history behind it.
			model.RecordChannelObservation(channel.Id, model.ChannelObservation{
				Kind:      classifyProbeFailure(openaiErr, err),
				LatencyMs: float64(milliseconds),
			})

			if !isChannelEnabled && err == nil && openaiErr == nil {
				// Prefer the health engine's stricter gate: it requires
				// repeated clean probes rather than one, so a provider that
				// briefly stopped answering cannot be immediately put back
				// into rotation the moment it answers once.
				if recovered, why := model.ShouldAutoRecoverChannel(channel.Id); recovered {
					lg.Info("auto-disabled channel recovered by channel test",
						zap.Int("channel_id", channel.Id),
						zap.String("channel_name", channel.Name),
						zap.Int64("latency_ms", milliseconds),
					)
					monitor.EnableChannel(channel.Id, channel.Name)
				} else if channel.Status == model.ChannelStatusAutoDisabled {
					lg.Debug("auto-disabled channel still awaiting recovery",
						zap.Int("channel_id", channel.Id),
						zap.String("reason", why),
					)
				}
			} else if !isChannelEnabled && (err == nil && monitor.ShouldEnableChannel(err, openaiErr)) {
				monitor.EnableChannel(channel.Id, channel.Name)
			}
			channel.UpdateResponseTime(milliseconds)
			time.Sleep(config.RequestInterval)
		}
		testAllChannelsLock.Lock()
		testAllChannelsRunning = false
		testAllChannelsLock.Unlock()
		if notify {
			err := message.Notify(message.ByAll, "Channel test completed", "", "Channel test completed, if you have not received the disable notification, it means that all channels are normal")
			if err != nil {
				lg.Error("failed to send notify", zap.Error(err))
			}
		}
	}()
	return nil
}

// TestChannels initiates a background test sweep across a set of channels defined by scope.
func TestChannels(c *gin.Context) {
	ctx := gmw.Ctx(c)
	scope := c.Query("scope")
	if scope == "" {
		scope = "all"
	}
	err := testChannels(ctx, true, scope)
	if err != nil {
		helper.RespondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

// AutomaticallyTestChannels continuously runs channel tests at the provided interval in minutes.
func AutomaticallyTestChannels(frequency int) {
	lg := logger.Logger.Named("auto_test_channels")
	ctx := context.Background()
	ctx = gmw.SetLogger(ctx, lg)

	for {
		time.Sleep(time.Duration(frequency) * time.Minute)
		lg.Info("testing auto-disabled channels")
		_ = testChannels(ctx, false, "auto_disabled")
		lg.Info("channel test finished")
	}
}
