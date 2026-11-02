package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (a *App) translationProxy(c *gin.Context, name string, profile TranslationProfile, forceNonStream bool) {
	rawContent, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "details": "读取请求体失败"})
		return
	}

	log.Printf("%s 原始请求内容: %s", name, string(rawContent))
	log.Printf("请求 Content-Type: %s", c.ContentType())
	if !strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "application/json") {
		log.Printf("警告: Content-Type 不是 application/json: %s", c.GetHeader("Content-Type"))
	}

	var incoming map[string]interface{}
	if err := json.Unmarshal(rawContent, &incoming); err != nil || incoming == nil {
		errorMsg := fmt.Sprintf("请求中不包含有效的 JSON。原始内容: %s...", truncate(string(rawContent), 200))
		log.Printf("JSON解析失败: %s", errorMsg)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errorMsg,
			"details": "请检查请求格式是否正确",
			"hint":    "确保请求头包含 'Content-Type: application/json'",
		})
		return
	}

	log.Printf("解析后的请求数据: %+v", incoming)

	messages, ok := incoming["messages"].([]interface{})
	if !ok {
		messages = nil
	}
	log.Printf("Messages 数量: %d", len(messages))

	userMessageText := extractUserMessageText(messages)
	if userMessageText == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":    "Empty message",
			"details":  "无法从请求中提取有效的用户消息内容",
			"messages": messages,
		})
		return
	}

	normalizedWordText := normalizeWordText(userMessageText)
	isWordInput := isWord(userMessageText)
	effectiveUserText := userMessageText
	if isWordInput {
		effectiveUserText = normalizedWordText
	}
	log.Printf("是否为单词输入: %t, 原文本: %q, 规范化后: %q", isWordInput, userMessageText, effectiveUserText)

	outgoing, err := a.buildOutgoingPayload(incoming, profile, isWordInput, userTextOverride(isWordInput, effectiveUserText))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if forceNonStream {
		outgoing["stream"] = false
	}
	if _, ok := outgoing["model"]; !ok || outgoing["model"] == "" {
		outgoing["model"] = defaultModel
	}
	if _, ok := outgoing["messages"]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid messages format", "details": "构造的payload中缺少messages"})
		return
	}

	log.Printf("构造的payload: %+v", outgoing)

	word := ""
	if isWordInput {
		word = effectiveUserText
	}
	a.proxyDeepSeek(c, outgoing, profile.TargetLanguage, word)
}
