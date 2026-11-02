package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyDeepSeekDatabaseCacheHitDoesNotCallUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		stream bool
		apiKey string
	}{
		{name: "non-stream empty API key", stream: false, apiKey: ""},
		{name: "non-stream invalid API key", stream: false, apiKey: "invalid-key"},
		{name: "stream empty API key", stream: true, apiKey: ""},
		{name: "stream invalid API key", stream: true, apiKey: "invalid-key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DEEPSEEK_API_KEY", "")

			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				http.Error(w, "upstream must not be called", http.StatusUnauthorized)
			}))
			t.Cleanup(upstream.Close)

			db, mock := newMockGormDB(t)
			expectWordCacheHit(mock, "zh-CN", "example", "例子")

			localConfig := &LocalConfig{}
			localConfig.Relay.APIKey = tt.apiKey
			localConfig.Relay.URL = upstream.URL
			app := newApp(defaultConfig(), localConfig, db)

			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = httptest.NewRequest(http.MethodPost, "/anx-reader", nil)

			app.proxyDeepSeek(context, map[string]interface{}{
				"model":  "DeepSeek-V3",
				"stream": tt.stream,
			}, "zh-CN", "example")

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			if calls := upstreamCalls.Load(); calls != 0 {
				t.Fatalf("upstream calls = %d, want 0", calls)
			}

			body := response.Body.String()
			if !strings.Contains(body, "例子") {
				t.Fatalf("response does not contain cached translation: %q", body)
			}
			if tt.stream {
				if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
					t.Fatalf("Content-Type = %q, want text/event-stream", got)
				}
				if !strings.HasSuffix(body, "data: [DONE]\n\n") {
					t.Fatalf("cached SSE response must end with [DONE], got %q", body)
				}
			} else if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("database expectations: %v", err)
			}
		})
	}
}

func TestTranslationRouteReturnsDatabaseCacheWithInvalidAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("DEEPSEEK_API_KEY", "")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "upstream must not be called", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)

	db, mock := newMockGormDB(t)
	expectWordCacheHit(mock, "zh-CN", "example", "例子")

	localConfig := &LocalConfig{}
	localConfig.Relay.APIKey = "invalid-key"
	localConfig.Relay.URL = upstream.URL
	app := newApp(defaultConfig(), localConfig, db)
	router := gin.New()
	app.registerRoutes(router)

	request := httptest.NewRequest(
		http.MethodPost,
		"/anx-reader",
		strings.NewReader("{\"messages\":[{\"role\":\"user\",\"content\":\"Example\"}],\"stream\":false}"),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "例子") {
		t.Fatalf("response does not contain cached translation: %q", response.Body.String())
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestProxyDeepSeekEmptyAPIKeyCacheMissReturnsServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("DEEPSEEK_API_KEY", "")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "upstream must not be called", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)

	db, mock := newMockGormDB(t)
	expectWordCacheMiss(mock, "zh-CN", "missing")

	localConfig := &LocalConfig{}
	localConfig.Relay.URL = upstream.URL
	app := newApp(defaultConfig(), localConfig, db)

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/anx-reader", nil)
	app.proxyDeepSeek(context, map[string]interface{}{"stream": false}, "zh-CN", "missing")

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "upstream_unavailable") {
		t.Fatalf("body = %q, want upstream_unavailable", response.Body.String())
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestProxyDeepSeekInvalidAPIKeyCacheMissCallsUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("DEEPSEEK_API_KEY", "")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		if got := request.Header.Get("Authorization"); got != "Bearer invalid-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer invalid-key")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("{\"error\":\"invalid_api_key\"}"))
	}))
	t.Cleanup(upstream.Close)

	db, mock := newMockGormDB(t)
	expectWordCacheMiss(mock, "zh-CN", "missing")

	localConfig := &LocalConfig{}
	localConfig.Relay.APIKey = "invalid-key"
	localConfig.Relay.URL = upstream.URL
	app := newApp(defaultConfig(), localConfig, db)

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/anx-reader", nil)
	app.proxyDeepSeek(context, map[string]interface{}{"stream": false}, "zh-CN", "missing")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestProxyDeepSeekDatabaseErrorFailsOpenToUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("DEEPSEEK_API_KEY", "")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("{\"error\":\"invalid_api_key\"}"))
	}))
	t.Cleanup(upstream.Close)

	db, mock := newMockGormDB(t)
	mock.ExpectQuery(wordCacheLookupQuery).
		WithArgs("zh-CN", "example", 1).
		WillReturnError(io.ErrUnexpectedEOF)

	localConfig := &LocalConfig{}
	localConfig.Relay.APIKey = "invalid-key"
	localConfig.Relay.URL = upstream.URL
	app := newApp(defaultConfig(), localConfig, db)

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/anx-reader", nil)
	app.proxyDeepSeek(context, map[string]interface{}{"stream": false}, "zh-CN", "example")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestProxyDeepSeekCacheDisabledBypassesExistingMemoryCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("DEEPSEEK_API_KEY", "")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("{\"error\":\"invalid_api_key\"}"))
	}))
	t.Cleanup(upstream.Close)

	cacheDisabled := false
	localConfig := &LocalConfig{}
	localConfig.Relay.APIKey = "invalid-key"
	localConfig.Relay.URL = upstream.URL
	localConfig.Relay.Cache = &cacheDisabled
	app := newApp(defaultConfig(), localConfig, nil)
	app.wordCache.Store(wordCacheKey("zh-CN", "example"), "例子")

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/anx-reader", nil)
	app.proxyDeepSeek(context, map[string]interface{}{"stream": false}, "zh-CN", "example")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestProxyStreamRelaysSSEEventBoundariesAndDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	upstream := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"Kamusta\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}

	newApp(defaultConfig(), nil, nil).proxyStream(context, upstream, "tl", "", false)

	if got, want := response.Header().Get("Content-Type"), "text/event-stream; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"Kamusta\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
		"data: [DONE]\n\n"
	if got := response.Body.String(); got != want {
		t.Fatalf("SSE response = %q, want %q", got, want)
	}
}

func TestProxyStreamAppendsDoneWhenUpstreamOmitsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	upstream := &http.Response{
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"halimbawa\"},\"finish_reason\":\"stop\"}]}\n\n",
		)),
	}

	newApp(defaultConfig(), nil, nil).proxyStream(context, upstream, "tl", "", false)

	want := "data: {\"choices\":[{\"delta\":{\"content\":\"halimbawa\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	if got := response.Body.String(); got != want {
		t.Fatalf("SSE response = %q, want %q", got, want)
	}
}

func TestWriteCachedStreamEndsWithDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)

	newApp(defaultConfig(), nil, nil).writeCachedStream(context, map[string]interface{}{
		"model": "DeepSeek-V3",
	}, "halimbawa")

	body := response.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("cached SSE response must end with [DONE], got %q", body)
	}
	if !strings.Contains(body, `"content":"halimbawa"`) {
		t.Fatalf("cached SSE response must include cached translation, got %q", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("cached SSE response must include assistant role, got %q", body)
	}
}
