package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type execResultMeta struct {
	ExitCode int `json:"exit_code"`
}

// cmdStderrTerm runs a Cmd, collecting stderr and terminating gracefully
func cmdStderrTerm(ctx context.Context, logger log.FieldLogger, stderrPath string, metaPath string, cmd *exec.Cmd) error {
	var out bytes.Buffer
	var meta execResultMeta
	cmd.Stderr = &out

	err := cmdRunTerm(ctx, logger, cmd)

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitErr.Stderr = out.Bytes()
		meta.ExitCode = exitErr.ExitCode()
	}

	if mkdirErr := os.MkdirAll(filepath.Dir(stderrPath), 0755); mkdirErr != nil {
		if err != nil {
			logger.Errorf("Failed to create directory for stderr; suppressing original error: %v\n", err)
		}
		return mkdirErr
	}

	if stderrWriteErr := os.WriteFile(stderrPath, out.Bytes(), 0644); stderrWriteErr != nil {
		if err != nil {
			logger.Errorf("Failed to write stderr; suppressing original error: %v\n", err)
		}
		return stderrWriteErr
	}

	if metaPath != "" {
		if mkdirErr := os.MkdirAll(filepath.Dir(metaPath), 0755); mkdirErr != nil {
			if err != nil {
				logger.Errorf("Failed to create directory for metadata; suppressing original error: %v\n", err)
			}
			return mkdirErr
		}
		metaBytes, jsonErr := json.Marshal(meta)
		if jsonErr != nil {
			if err != nil {
				logger.Errorf("Failed to marshal meta; suppressing original error: %v\n", err)
			}
			return jsonErr
		}
		if metaWriteErr := os.WriteFile(metaPath, metaBytes, 0644); metaWriteErr != nil {
			if err != nil {
				logger.Errorf("Failed to write meta; suppressing original error: %v\n", err)
			}
			return metaWriteErr
		}
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
