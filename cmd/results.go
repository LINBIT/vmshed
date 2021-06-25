package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

func saveResultsJSON(suiteRun testSuiteRun, startTime time.Time, results map[string]testResult) error {
	type resultData struct {
		ID         string    `json:"id"`
		Time       time.Time `json:"time"`
		Name       string    `json:"name"`
		VMCount    int       `json:"vm_count"`
		Variant    string    `json:"variant"`
		BaseImages []string  `json:"base_images"`
		Status     string    `json:"status"`
		Score      int       `json:"score"`
		DurationNS int64     `json:"duration_ns"`
	}

	filename := filepath.Join(suiteRun.outDir, "results.json")
	log.Infof("Saving results as JSON to %s", filename)
	dest, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("Failed to create results JSON file: %w", err)
	}
	defer dest.Close()

	enc := json.NewEncoder(dest)

	for _, testRun := range suiteRun.testRuns {
		result, ok := results[testRun.testID]
		if !ok {
			// exclude skipped runs
			continue
		}
		if result.status == StatusCanceled {
			// exclude canceled runs
			continue
		}

		data := resultData{
			ID: testRun.testID,
			// record all results from the test suite run with time from the start
			Time:       startTime,
			Name:       testRun.testName,
			VMCount:    len(testRun.vms),
			Variant:    testRun.variant.Name,
			BaseImages: baseImageNames(testRun.vms),
			Status:     string(result.status),
			Score:      statusScore(result.status),
			DurationNS: result.execTime.Nanoseconds(),
		}

		if err := enc.Encode(&data); err != nil {
			return fmt.Errorf("failed to encode results JSON: %w", err)
		}
	}

	return dest.Sync()
}

func baseImageNames(vms []vm) []string {
	names := []string{}
	for _, v := range vms {
		names = append(names, v.BaseImage)
	}
	return names
}

func statusScore(s TestStatus) int {
	if s == StatusSuccess {
		return 1
	}
	return 0
}
