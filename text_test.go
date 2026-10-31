package main

import "testing"

func TestNormalizeWordTextCutsBulletSuffix(t *testing.T) {
	input := "pictures  • 新郎/新娘的伴郎伴娘团摆姿势拍照"

	if got := normalizeWordText(input); got != "pictures" {
		t.Fatalf("normalizeWordText() = %q, want %q", got, "pictures")
	}
}

func TestIsWordCutsBulletSuffix(t *testing.T) {
	input := "pictures  • 新郎/新娘的伴郎伴娘团摆姿势拍照"

	if !isWord(input) {
		t.Fatalf("isWord() = false, want true")
	}
}

func TestNormalizeWordTextCutsZoteroChineseTitleAfterSpaces(t *testing.T) {
	input := "Knowing  两种认知系统"

	if got := normalizeWordText(input); got != "Knowing" {
		t.Fatalf("normalizeWordText() = %q, want %q", got, "Knowing")
	}
	if !isWord(input) {
		t.Fatalf("isWord() = false, want true")
	}
}

func TestNormalizeWordTextCutsZoteroChineseTitleAfterNewline(t *testing.T) {
	input := "Learning\r\n生成性学习的一个例子"

	if got := normalizeWordText(input); got != "Learning" {
		t.Fatalf("normalizeWordText() = %q, want %q", got, "Learning")
	}
}

func TestNormalizeWordTextDoesNotCutSingleSpace(t *testing.T) {
	input := "Knowing 两种认知系统"

	if got := normalizeWordText(input); got != input {
		t.Fatalf("normalizeWordText() = %q, want %q", got, input)
	}
	if isWord(input) {
		t.Fatalf("isWord() = true, want false")
	}
}

func TestNormalizeWordTextDoesNotCutEnglishContinuation(t *testing.T) {
	input := "Knowing  systems"

	if got := normalizeWordText(input); got != input {
		t.Fatalf("normalizeWordText() = %q, want %q", got, input)
	}
}
