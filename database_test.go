package main

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const wordCacheLookupQuery = "(?i)SELECT .*translation_result.* FROM .*word_cache.* WHERE target_language = \\? AND word = \\?.*LIMIT \\?"

func newMockGormDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	return db, mock
}

func expectWordCacheHit(mock sqlmock.Sqlmock, targetLanguage, word, translation string) {
	mock.ExpectQuery(wordCacheLookupQuery).
		WithArgs(targetLanguage, word, 1).
		WillReturnRows(sqlmock.NewRows([]string{"translation_result"}).AddRow(translation))
}

func expectWordCacheMiss(mock sqlmock.Sqlmock, targetLanguage, word string) {
	mock.ExpectQuery(wordCacheLookupQuery).
		WithArgs(targetLanguage, word, 1).
		WillReturnRows(sqlmock.NewRows([]string{"translation_result"}))
}

func TestGetCachedWordLoadsDatabaseAndThenMemory(t *testing.T) {
	db, mock := newMockGormDB(t)
	app := newApp(defaultConfig(), nil, db)
	expectWordCacheHit(mock, "zh-CN", "example", "例子")

	content, found, err := app.getCachedWord(context.Background(), "zh-CN", " Example ")
	if err != nil {
		t.Fatalf("getCachedWord() error = %v", err)
	}
	if !found || content != "例子" {
		t.Fatalf("getCachedWord() = (%q, %t), want (%q, true)", content, found, "例子")
	}

	// The second lookup must be served by sync.Map without another SQL query.
	content, found, err = app.getCachedWord(context.Background(), "zh-CN", "EXAMPLE")
	if err != nil {
		t.Fatalf("second getCachedWord() error = %v", err)
	}
	if !found || content != "例子" {
		t.Fatalf("second getCachedWord() = (%q, %t), want (%q, true)", content, found, "例子")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestGetCachedWordDistinguishesMissFromDatabaseError(t *testing.T) {
	t.Run("miss", func(t *testing.T) {
		db, mock := newMockGormDB(t)
		app := newApp(defaultConfig(), nil, db)
		expectWordCacheMiss(mock, "zh-CN", "missing")

		content, found, err := app.getCachedWord(context.Background(), "zh-CN", "missing")
		if err != nil {
			t.Fatalf("getCachedWord() error = %v", err)
		}
		if found || content != "" {
			t.Fatalf("getCachedWord() = (%q, %t), want empty miss", content, found)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("database expectations: %v", err)
		}
	})

	t.Run("database error", func(t *testing.T) {
		db, mock := newMockGormDB(t)
		app := newApp(defaultConfig(), nil, db)
		wantErr := errors.New("database unavailable")
		mock.ExpectQuery(wordCacheLookupQuery).
			WithArgs("zh-CN", "example", 1).
			WillReturnError(wantErr)

		content, found, err := app.getCachedWord(context.Background(), "zh-CN", "example")
		if !errors.Is(err, wantErr) {
			t.Fatalf("getCachedWord() error = %v, want %v", err, wantErr)
		}
		if found || content != "" {
			t.Fatalf("getCachedWord() = (%q, %t), want empty error result", content, found)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("database expectations: %v", err)
		}
	})
}

func TestGetCachedWordUsesMemoryWhenDatabaseIsUnavailable(t *testing.T) {
	app := newApp(defaultConfig(), nil, nil)
	app.wordCache.Store(wordCacheKey("zh-CN", "example"), "例子")

	content, found, err := app.getCachedWord(context.Background(), "zh-CN", "example")
	if err != nil {
		t.Fatalf("getCachedWord() error = %v", err)
	}
	if !found || content != "例子" {
		t.Fatalf("getCachedWord() = (%q, %t), want (%q, true)", content, found, "例子")
	}
}

func TestGetCachedWordDoesNotReuseAnotherLanguage(t *testing.T) {
	db, mock := newMockGormDB(t)
	app := newApp(defaultConfig(), nil, db)
	app.wordCache.Store(wordCacheKey("zh-CN", "example"), "例子")
	expectWordCacheMiss(mock, "tl", "example")

	content, found, err := app.getCachedWord(context.Background(), "tl", "example")
	if err != nil {
		t.Fatalf("getCachedWord() error = %v", err)
	}
	if found || content != "" {
		t.Fatalf("Tagalog getCachedWord() = (%q, %t), want empty miss", content, found)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestCacheEnabledDefaultsToTrue(t *testing.T) {
	if !newApp(defaultConfig(), nil, nil).cacheEnabled() {
		t.Fatal("cacheEnabled() = false without local config, want true")
	}

	localConfig := &LocalConfig{}
	disabled := false
	localConfig.Relay.Cache = &disabled
	if newApp(defaultConfig(), localConfig, nil).cacheEnabled() {
		t.Fatal("cacheEnabled() = true with Relay.Cache=false, want false")
	}
}
