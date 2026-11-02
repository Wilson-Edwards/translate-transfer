package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type DatabaseConfig struct {
	Type     string                 `yaml:"Type"`
	Path     string                 `yaml:"Path"`
	Port     interface{}            `yaml:"Port"`
	Config   string                 `yaml:"Config"`
	Dbname   string                 `yaml:"Dbname"`
	Username string                 `yaml:"Username"`
	Password string                 `yaml:"Password"`
	Driver   string                 `yaml:"Driver"`
	Mysql    map[string]interface{} `yaml:"Mysql"`
}

type WordCache struct {
	ID                uint      `gorm:"primaryKey;autoIncrement;column:id"`
	Word              string    `gorm:"column:word;type:varchar(100);not null;uniqueIndex:uniq_target_language_word;index:idx_word"`
	TargetLanguage    string    `gorm:"column:target_language;type:varchar(16);not null;uniqueIndex:uniq_target_language_word"`
	TranslationResult string    `gorm:"column:translation_result;type:text;not null"`
	CreatedAt         time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt         time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (WordCache) TableName() string {
	return "word_cache"
}

func initDB(localConfig *LocalConfig) (*gorm.DB, error) {
	dbCfg := DatabaseConfig{Type: "mysql"}
	if localConfig != nil {
		dbCfg = localConfig.Database
		if strings.TrimSpace(dbCfg.Type) == "" {
			dbCfg.Type = "mysql"
		}
	}

	dbType := strings.ToLower(strings.TrimSpace(dbCfg.Type))
	var dialector gorm.Dialector

	switch dbType {
	case "", "mysql":
		mysqlCfg := dbCfg.Mysql
		host := firstNonEmpty(getString(mysqlCfg, "Host", ""), dbCfg.Path, "127.0.0.1")
		port := toInt(firstValue(getValue(mysqlCfg, "Port"), dbCfg.Port), 3306)
		dbname := firstNonEmpty(getString(mysqlCfg, "Dbname", ""), dbCfg.Dbname, "translate_transfer")
		username := firstNonEmpty(getString(mysqlCfg, "Username", ""), dbCfg.Username, "root")
		password := firstNonEmptyAllowBlank("", getStringPtr(mysqlCfg, "Password"), &dbCfg.Password)
		params := firstNonEmpty(getString(mysqlCfg, "Params", ""), dbCfg.Config, "charset=utf8mb4&parseTime=True&loc=Local")
		if err := ensureMySQLDatabase(host, port, username, password, dbname, params); err != nil {
			return nil, err
		}
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", url.QueryEscape(username), url.QueryEscape(password), host, port, dbname)
		if params != "" {
			dsn += "?" + strings.TrimPrefix(params, "?")
		}
		dialector = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("Go 版本仅支持 MySQL，请将 local.yaml 中 Database.Type 设置为 mysql，当前为 %q", dbType)
	}

	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&WordCache{}); err != nil {
		return nil, err
	}
	return db, nil
}

func ensureMySQLDatabase(host string, port int, username, password, dbname, params string) error {
	if strings.TrimSpace(dbname) == "" {
		return errors.New("MySQL 数据库名不能为空")
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/", url.QueryEscape(username), url.QueryEscape(password), host, port)
	if params != "" {
		dsn += "?" + strings.TrimPrefix(params, "?")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return err
	}

	quotedName := "`" + strings.ReplaceAll(dbname, "`", "``") + "`"
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + quotedName + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return fmt.Errorf("创建 MySQL 数据库 %q 失败: %w", dbname, err)
	}
	return nil
}

func (a *App) cacheEnabled() bool {
	return a.localConfig == nil || a.localConfig.Relay.Cache == nil || *a.localConfig.Relay.Cache
}

func wordCacheKey(targetLanguage, word string) string {
	return strings.ToLower(strings.TrimSpace(targetLanguage)) + ":" + strings.ToLower(strings.TrimSpace(word))
}

func (a *App) getCachedWord(ctx context.Context, targetLanguage, word string) (string, bool, error) {
	targetLanguage = strings.TrimSpace(targetLanguage)
	lowerWord := strings.ToLower(strings.TrimSpace(word))
	cacheKey := wordCacheKey(targetLanguage, lowerWord)
	if value, ok := a.wordCache.Load(cacheKey); ok {
		if translation, ok := value.(string); ok {
			return translation, true, nil
		}
	}

	if a.db == nil {
		return "", false, errors.New("word cache database is not initialized")
	}

	var cache WordCache
	err := a.db.WithContext(ctx).
		Select("translation_result").
		Where("target_language = ? AND word = ?", targetLanguage, lowerWord).
		Take(&cache).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(cache.TranslationResult) == "" {
		log.Printf("缓存数据为空: target_language=%q word=%q", targetLanguage, lowerWord)
		return "", false, nil
	}

	a.wordCache.Store(cacheKey, cache.TranslationResult)
	return cache.TranslationResult, true, nil
}

func (a *App) cacheWordTranslation(targetLanguage, word, translationResult string) bool {
	if a.db == nil {
		return false
	}

	targetLanguage = strings.TrimSpace(targetLanguage)
	lowerWord := strings.ToLower(strings.TrimSpace(word))
	cacheKey := wordCacheKey(targetLanguage, lowerWord)
	var cache WordCache
	err := a.db.WithContext(context.Background()).Where("target_language = ? AND word = ?", targetLanguage, lowerWord).First(&cache).Error
	if err == nil {
		cache.TranslationResult = translationResult
		if err := a.db.Save(&cache).Error; err != nil {
			log.Printf("数据库错误: %v", err)
			return false
		}
		a.wordCache.Store(cacheKey, translationResult)
		return true
	}

	if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("数据库错误: %v", err)
		return false
	}

	cache = WordCache{
		Word:              lowerWord,
		TargetLanguage:    targetLanguage,
		TranslationResult: translationResult,
	}
	if err := a.db.Create(&cache).Error; err != nil {
		log.Printf("数据库错误: %v", err)
		return false
	}
	a.wordCache.Store(cacheKey, translationResult)
	return true
}
