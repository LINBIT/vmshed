package cmd

import (
	"io"
	"sort"

	log "github.com/sirupsen/logrus"
)

// VmshedStandardLogFormatter creates a Formatter with vmshed specific customizations.
func VmshedStandardLogFormatter() *log.TextFormatter {
	return &log.TextFormatter{
		TimestampFormat: "2006-01-02 15:04:05.000",
		SortingFunc:     logKeySort,
	}
}

// logKeySort sorts with a fixed set of keys being preferred.
func logKeySort(keys []string) {
	sort.Sort(BiasedStringSlice(keys))
}

// BiasedStringSlice implements sort.Interface with a fixed set of preferred strings.
type BiasedStringSlice []string

func (s BiasedStringSlice) Len() int {
	return len(s)
}

func (s BiasedStringSlice) Less(i, j int) bool {
	iStr := s[i]
	jStr := s[j]
	iPref, iFixed := fixedKeys[iStr]
	jPref, jFixed := fixedKeys[jStr]

	if iFixed {
		if jFixed {
			return iPref < jPref
		} else {
			return true
		}
	} else {
		if jFixed {
			return false
		} else {
			return sort.StringSlice(s).Less(i, j)
		}
	}
}

func (s BiasedStringSlice) Swap(i, j int) {
	sort.StringSlice(s).Swap(i, j)
}

var fixedKeys = map[string]int{
	log.FieldKeyTime:  1,
	log.FieldKeyLevel: 2,
	log.FieldKeyFile:  3,
	log.FieldKeyFunc:  4,
	logFieldKeyID:     5,
}

// TestLogger creates a Logger for use in test runs. Logs will be written to
// the standard logger, with the ID attached, and to the given Writer, without
// the ID.
func TestLogger(testID string, out io.Writer) *log.Logger {
	logger := log.New()
	logger.Out = out
	logger.Level = log.DebugLevel
	logger.Formatter = &log.TextFormatter{
		DisableQuote:    true,
		TimestampFormat: "15:04:05.000",
	}

	logger.AddHook(&StandardLoggerHook{testID: testID})
	return logger
}

// StandardLoggerHook duplicates log messages to the standard logger, adding an ID field
type StandardLoggerHook struct {
	testID string
}

func (hook *StandardLoggerHook) Fire(entry *log.Entry) error {
	logEntry := *entry
	logEntry.Logger = log.StandardLogger()
	logEntry.Data[logFieldKeyID] = hook.testID
	logEntry.Log(logEntry.Level, logEntry.Message)
	// Delete the ID field again, otherwise the other logger will include it.
	delete(entry.Data, logFieldKeyID)
	return nil
}

// Intercept all levels, whether or not they are actually output.
func (hook *StandardLoggerHook) Levels() []log.Level {
	return log.AllLevels
}

const logFieldKeyID string = "id"
