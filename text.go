package main

import (
	"log"
	"regexp"
	"strings"
	"unicode"
)

var (
	wordPattern                    = regexp.MustCompile(`^[a-zA-Z]{1,50}$`)
	sourcePattern                  = regexp.MustCompile(`sourceText:\s*(.+?)(?:\n|$)`)
	zoteroTitleContinuationPattern = regexp.MustCompile(`(?:[ \t]{2,}|[ \t]*\r?\n[ \t]*)`)
)

func extractUserMessageText(messages []interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		log.Printf("检查消息 %d: %+v", len(messages)-1-i, messages[i])
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		content, _ := msg["content"].(string)
		userMessageText := strings.TrimSpace(content)
		log.Printf("找到用户消息: %q", userMessageText)

		if match := sourcePattern.FindStringSubmatch(userMessageText); len(match) > 1 {
			actualWord := strings.TrimSpace(match[1])
			log.Printf("提取到的实际单词: %q", actualWord)
			userMessageText = actualWord
		}
		return userMessageText
	}

	return ""
}

func normalizeWordText(text string) string {
	text = strings.TrimSpace(text)
	if beforeBullet, _, found := strings.Cut(text, "•"); found {
		text = beforeBullet
	}
	text = trimZoteroTitleContinuation(text)

	var b strings.Builder
	for _, r := range text {
		if unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// trimZoteroTitleContinuation handles a Zotero selection edge case: selecting
// the final English word on a title line can also include the Chinese title on
// the following line. Depending on the client, the line break arrives either
// as a real newline or as two or more spaces.
func trimZoteroTitleContinuation(text string) string {
	separator := zoteroTitleContinuationPattern.FindStringIndex(text)
	if separator == nil {
		return text
	}

	prefix := strings.TrimSpace(text[:separator[0]])
	suffix := text[separator[1]:]
	if !wordPattern.MatchString(prefix) || !containsHan(suffix) {
		return text
	}

	return prefix
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func isWord(text string) bool {
	return wordPattern.MatchString(normalizeWordText(text))
}

func userTextOverride(enabled bool, text string) *string {
	if !enabled {
		return nil
	}
	return &text
}
