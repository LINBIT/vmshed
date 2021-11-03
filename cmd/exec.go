package cmd

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// cmdStderrTerm runs a Cmd, collecting stderr and terminating gracefully
func cmdStderrTerm(ctx context.Context, logger log.FieldLogger, stderrPath string, cmd *exec.Cmd) error {
	var out bytes.Buffer
	cmd.Stderr = &out

	err := cmdRunTerm(ctx, logger, cmd)

	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = out.Bytes()
	}

	if mkdirErr := os.MkdirAll(filepath.Dir(stderrPath), 0755); mkdirErr != nil {
		if err != nil {
			logger.Errorf("Failed to create directory for stderr; suppressing original error: %v\n", err)
		}
		return mkdirErr
	}

	if stderrWriteErr := ioutil.WriteFile(stderrPath, out.Bytes(), 0644); stderrWriteErr != nil {
		if err != nil {
			logger.Errorf("Failed to write stderr; suppressing original error: %v\n", err)
		}
		return stderrWriteErr
	}

	return err
}

// cmdRunTerm runs a Cmd, terminating it gracefully when the context is done
func cmdRunTerm(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		return err
	}

	complete := make(chan struct{})
	finished := make(chan struct{})

	go handleTermination(ctx, logger, cmd, complete, finished)

	err = cmd.Wait()

	// Inform the termination handler that it can stop
	close(complete)

	// Wait for the termination handler to stop, so that the context can be
	// cancelled without risk of sending an extra signal
	<-finished

	return err
}

func handleTermination(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd, complete <-chan struct{}, finished chan<- struct{}) {
	select {
	case <-ctx.Done():
		logger.Warnln("TERMINATING: Send SIGTERM")
		cmd.Process.Signal(unix.SIGTERM)
		select {
		case <-time.After(30 * time.Second):
			logger.Errorln("TERMINATING: Send SIGKILL")
			cmd.Process.Kill()
		case <-complete:
		}
	case <-complete:
	}
	close(finished)
}
