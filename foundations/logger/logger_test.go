package logger

import (
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewLogger(t *testing.T) {
	is := is.New(t)

	log, err := NewLogger()
	is.NoErr(err)
	_ = log.Sync()
}

func TestLoggerNilSafeMethods(t *testing.T) {
	var log Logger

	log.Info("info", WithFields(String("key", "value"), Bool("ok", true), Duration("timeout", time.Second)))
	log.Error("error", WithFields(String("key", "value")))
	_ = log.Sync()
}

func TestLoggerWritesStructuredFields(t *testing.T) {
	is := is.New(t)

	core, observedLogs := observer.New(zapcore.InfoLevel)
	log := newLogger(zap.New(core))
	at := time.Date(2026, time.May, 7, 12, 0, 0, 0, time.UTC)
	errExample := errors.New("boom")

	log.Info("structured_log", WithFields(
		String("name", "borq"),
		Bool("enabled", true),
		Duration("timeout", 2*time.Second),
		Time("at", at),
		Int("count", 3),
		Float64("ratio", 1.5),
		Any("problem", errExample),
	))

	entries := observedLogs.All()
	is.Equal(len(entries), 1)
	is.Equal(entries[0].Message, "")

	context := entries[0].ContextMap()
	is.Equal(context["event"], "structured_log")
	is.Equal(context["name"], "borq")
	is.Equal(context["enabled"], true)
	is.Equal(context["count"], int64(3))
	is.Equal(context["ratio"], 1.5)
	is.Equal(context["problem"], errExample.Error())
	is.Equal(context["at"], at)
}

func TestLoggerWithAnyRendersWithVerb(t *testing.T) {
	is := is.New(t)

	core, observedLogs := observer.New(zapcore.InfoLevel)
	log := newLogger(zap.New(core))

	type custom struct{ Name string }
	log.Info("any_test", WithFields(Any("custom", custom{Name: "borq"})))

	entries := observedLogs.All()
	is.Equal(len(entries), 1)
	is.Equal(entries[0].ContextMap()["custom"], "{Name:borq}")
}
