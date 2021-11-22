package tracer

import (
	"log"

	sdklog "go.temporal.io/sdk/log"
)

var DefaultLogger sdklog.Logger = LoggerFunc(func(level, msg string, keyVals ...interface{}) {
	log.Println(append([]interface{}{level, msg}, keyVals...)...)
})

type LoggerFunc func(level, msg string, keyVals ...interface{})

func (l LoggerFunc) Debug(msg string, keyVals ...interface{}) {
	l("DEBUG", msg, keyVals)
}

func (l LoggerFunc) Info(msg string, keyVals ...interface{}) {
	l("INFO", msg, keyVals)
}

func (l LoggerFunc) Warn(msg string, keyVals ...interface{}) {
	l("WARN", msg, keyVals)
}

func (l LoggerFunc) Error(msg string, keyVals ...interface{}) {
	l("ERROR", msg, keyVals)
}
