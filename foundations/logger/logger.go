package logger

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger is the borq logging surface passed through the application.
type Logger struct {
	zapLogger *zap.Logger
}

// NewLogger creates the default borq CLI logger.
func NewLogger() (Logger, error) {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	config.Encoding = "json"
	config.OutputPaths = []string{"stderr"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.DisableStacktrace = true
	config.DisableCaller = true
	config.EncoderConfig.TimeKey = "time"
	config.EncoderConfig.LevelKey = "level"
	config.EncoderConfig.NameKey = ""
	config.EncoderConfig.CallerKey = ""
	config.EncoderConfig.MessageKey = ""
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

// Option is a functional option for configuring a log entry.
type Option interface {
	apply(*writeState)
}

// Field is a typed key-value pair for structured logging.
type Field struct {
	Key   string
	Value any
}

type writeState struct {
	fields []Field
}

// WithFields returns an Option that attaches the given fields.
func WithFields(fields ...Field) Option {
	return withFields(fields)
}

type withFields []Field

func (w withFields) apply(ws *writeState) {
	ws.fields = append(ws.fields, w...)
}

// String returns a Field with a string value.
func String(key, value string) Field { return Field{Key: key, Value: value} }

// Int returns a Field with an int value.
func Int(key string, value int) Field { return Field{Key: key, Value: value} }

// Bool returns a Field with a bool value.
func Bool(key string, value bool) Field { return Field{Key: key, Value: value} }

// Time returns a Field with a time.Time value.
func Time(key string, value time.Time) Field { return Field{Key: key, Value: value} }

// Duration returns a Field with a time.Duration value.
func Duration(key string, value time.Duration) Field { return Field{Key: key, Value: value} }

// Float64 returns a Field with a float64 value.
func Float64(key string, value float64) Field { return Field{Key: key, Value: value} }

// Any returns a Field for an arbitrary value, rendered as %+v.
func Any(key string, value any) Field { return Field{Key: key, Value: value} }

// Debug writes a debug log line.
func (logger Logger) Debug(msg string, opts ...Option) {
	logger.write(zapcore.DebugLevel, msg, opts)
}

// Info writes an info log line.
func (logger Logger) Info(msg string, opts ...Option) {
	logger.write(zapcore.InfoLevel, msg, opts)
}

// Error writes an error log line.
func (logger Logger) Error(msg string, opts ...Option) {
	logger.write(zapcore.ErrorLevel, msg, opts)
}

// Sync flushes buffered log entries.
func (logger Logger) Sync() error {
	return logger.zap().Sync()
}

func (logger Logger) write(level zapcore.Level, msg string, opts []Option) {
	ce := logger.zap().Check(level, "")
	if ce == nil {
		return
	}

	var ws writeState
	for _, opt := range opts {
		opt.apply(&ws)
	}

	zf := make([]zap.Field, 0, len(ws.fields)+1)
	zf = append(zf, zap.String("event", msg))
	for _, f := range ws.fields {
		zf = append(zf, toZapField(f.Key, f.Value))
	}

	ce.Write(zf...)
}

func (logger Logger) zap() *zap.Logger {
	if logger.zapLogger == nil {
		return zap.NewNop()
	}
	return logger.zapLogger
}

func newLogger(zapLogger *zap.Logger) Logger {
	return Logger{zapLogger: zapLogger}
}

func toZapField(key string, value any) zap.Field {
	switch v := value.(type) {
	case string:
		return zap.String(key, v)
	case int:
		return zap.Int(key, v)
	case bool:
		return zap.Bool(key, v)
	case time.Duration:
		return zap.Duration(key, v)
	case time.Time:
		return zap.Time(key, v)
	case float64:
		return zap.Float64(key, v)
	default:
		return zap.String(key, fmt.Sprintf("%+v", v))
	}
}
