package logger

import (
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestNewCliLogger(t *testing.T) {
	is := is.New(t)

	log, err := NewCliLogger()
	is.NoErr(err)
	_ = log.Sync()
}

func TestLoggerNamedAndNilSafeMethods(t *testing.T) {
	var log Logger

	log.Info("info", String("key", "value"), Bool("ok", true), Duration("timeout", time.Second))
	log.Error("error", String("key", "value"))
	_ = log.Sync()

	child := log.Named("child")
	child.Info("child-info")
	_ = child.Sync()
}
