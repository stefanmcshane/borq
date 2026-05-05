package logger

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Field is a borq logging field.
type Field struct {
	field zap.Field
}

// Logger is the borq logging surface passed through the application.
type Logger struct {
	zapLogger *zap.Logger
}

// NewCliLogger creates the default borq CLI logger.
func NewCliLogger() (Logger, error) {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	config.Encoding = "console"
	config.OutputPaths = []string{"stderr"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.DisableStacktrace = true
	config.DisableCaller = true
	config.EncoderConfig.TimeKey = "time"
	config.EncoderConfig.LevelKey = "level"
	config.EncoderConfig.NameKey = ""
	config.EncoderConfig.CallerKey = ""
	config.EncoderConfig.MessageKey = "msg"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	config.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	config.EncoderConfig.ConsoleSeparator = " "

	zapLogger, err := config.Build()
	if err != nil {
		return Logger{}, fmt.Errorf("build cli logger: %w", err)
	}

	return Logger{zapLogger: zapLogger}, nil
}

// Named returns a child logger with the supplied name.
func (logger Logger) Named(name string) Logger {
	return Logger{zapLogger: logger.zap().Named(name)}
}

// Info writes an info log line.
func (logger Logger) Info(message string, fields ...Field) {
	logger.zap().Info(message, zapFields(fields)...)
}

// Error writes an error log line.
func (logger Logger) Error(message string, fields ...Field) {
	logger.zap().Error(message, zapFields(fields)...)
}

// Sync flushes the logger.
func (logger Logger) Sync() error {
	return logger.zap().Sync()
}

// String creates a string field.
func String(key string, value string) Field {
	return Field{field: zap.String(key, value)}
}

// Bool creates a bool field.
func Bool(key string, value bool) Field {
	return Field{field: zap.Bool(key, value)}
}

// Duration creates a duration field.
func Duration(key string, value time.Duration) Field {
	return Field{field: zap.Duration(key, value)}
}

func zapFields(fields []Field) []zap.Field {
	zapFields := make([]zap.Field, 0, len(fields))
	for _, field := range fields {
		zapFields = append(zapFields, field.field)
	}

	return zapFields
}

func (logger Logger) zap() *zap.Logger {
	if logger.zapLogger == nil {
		return zap.NewNop()
	}

	return logger.zapLogger
}
