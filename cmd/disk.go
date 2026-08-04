package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// sharedDisk describes a disk that is attached to every VM of a test at the
// same time, for example to simulate a SAN attached volume.
type sharedDisk struct {
	Name string `toml:"name"`
	Size string `toml:"size"`
	Bus  string `toml:"bus"`
}

// sharedDiskInstance is a shared disk with a unique volume name for one test run.
type sharedDiskInstance struct {
	volumeName string
	size       string
	bus        string
}

func newSharedDiskInstance(disk sharedDisk, testID string) sharedDiskInstance {
	return sharedDiskInstance{
		// The test ID makes the volume name unique, assuming that only
		// one vmshed process uses the libvirt daemon at a time.
		volumeName: fmt.Sprintf("lbtest-disk-%s-%s", testID, disk.Name),
		size:       disk.Size,
		bus:        disk.Bus,
	}
}

// runArg returns the value for the "--shared-disk" argument of "virter vm run".
func (s sharedDiskInstance) runArg() string {
	arg := "name=" + s.volumeName
	if s.bus != "" {
		arg += ",bus=" + s.bus
	}
	return arg
}

func createSharedDisks(ctx context.Context, logger *log.Logger, outDir string, disks []sharedDiskInstance) error {
	for _, disk := range disks {
		// clean up, should not be neccessary, but hey...
		argv := []string{"virter", "disk", "rm", disk.volumeName}
		rmStderrPath := filepath.Join(outDir, fmt.Sprintf("pre_disk_rm_%s.log", disk.volumeName))
		logger.Debugf("EXECUTING: %s", argv)
		// this command is idempotent, so even if it does nothing, it returns zero
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmdStderrTerm(ctx, logger, rmStderrPath, "", cmd); err != nil {
			return err
		}

		argv = []string{"virter", "disk", "create", disk.volumeName, "--size", disk.size}
		stderrPath := filepath.Join(outDir, fmt.Sprintf("disk_create_%s.log", disk.volumeName))
		logger.Debugf("EXECUTING: %s", argv)
		cmd = exec.Command(argv[0], argv[1:]...)
		if err := cmdStderrTerm(ctx, logger, stderrPath, "", cmd); err != nil {
			return err
		}
	}

	return nil
}

func removeSharedDisks(logger *log.Logger, outDir string, res *testResult, suiteRun *testSuiteRun, disks []sharedDiskInstance) {
	if res.status == StatusFailed && suiteRun.onFailure == OnFailureKeepVms {
		logger.Warn("Test failed, leaving shared disks in place")
		logger.Info("Use \"virter disk rm ...\" when done with the disks")
		return
	}

	for _, disk := range disks {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		argv := []string{"virter", "disk", "rm", disk.volumeName}
		stderrPath := filepath.Join(outDir, fmt.Sprintf("disk_rm_%s.log", disk.volumeName))
		logger.Debugf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmdStderrTerm(ctx, logger, stderrPath, "", cmd); err != nil {
			logger.Errorf("ERROR: Could not remove shared disk %s: %v", disk.volumeName, err)
			dumpStderr(logger, err)
			// do not return, keep going...
		}
	}
}
