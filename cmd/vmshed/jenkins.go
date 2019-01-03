package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
)

type Jenkins struct {
	wsPath string
}

func checkWorkspacePath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("'%s' is not an absolute path", path)
	}
	if st, err := os.Stat(path); err != nil {
		return fmt.Errorf("Could not stat %s: %v", path, err)
	} else if !st.IsDir() {
		return fmt.Errorf("'%s' is not a directory", path)
	}

	return nil
}

// NewJenkins initializes a new Jenkins object and assiciates a Jenkins workspace with the object.
// An empty workspace string disables other functionality.
func NewJenkinsMust(workspacePath string) *Jenkins {

	if workspacePath != "" {
		if err := checkWorkspacePath(workspacePath); err != nil {
			log.Fatal(err)
		}
	}

	return &Jenkins{
		wsPath: workspacePath,
	}
}

// IsActive returnes true if a workspace path is set, otherwise false.
func (j *Jenkins) IsActive() bool { return j.wsPath != "" }

func (j *Jenkins) createSubDir(subdir string) (string, error) {
	if !j.IsActive() {
		return "", errors.New("This is not a jenkins run")
	}
	p := filepath.Join(j.wsPath, subdir)

	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}

	return p, os.MkdirAll(p, 0755)
}

// Log writes an arbitrary log file to, where "test" is a subdirectory in the Jenkins workspace, and "name" is the name of the file to write to
func (j *Jenkins) Log(test, name string, buf *bytes.Buffer) error {
	p, err := j.createSubDir(test)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filepath.Join(p, name), buf.Bytes(), 0644)
}

func (j *Jenkins) XMLLog(restultsDir, name string, testRes *testResult, buf *bytes.Buffer) error {
	// Used to remove invalid runes from the output.
	re, err := regexp.Compile("[^\t\n\r\x20-\x7e]")
	if err != nil {
		return err
	}

	p, err := j.createSubDir(restultsDir)

	f, err := os.Create(filepath.Join(p, name+".xml"))
	if err != nil {
		return err
	}
	defer f.Close()

	var nrFailed int
	if testRes.err != nil {
		nrFailed = 1 // currently there is only one test per execution
	}
	// header := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?><testsuite tests=\"1\" failures=\"0\" errors=\"%d\">\n", status)
	header := fmt.Sprintf("<testsuite tests=\"1\" failures=\"%d\" assertions=\"1\">\n", nrFailed)
	header += fmt.Sprintf("<testcase classname=\"test.%s\" name=\"%s.run\" time=\"%.2f\">", name, name, testRes.execTime.Seconds())
	header += "<system-out>\n<![CDATA[\n"
	f.WriteString(header)
	f.Write(re.ReplaceAllLiteral(buf.Bytes(), []byte{' '}))
	f.WriteString("]]></system-out>\n")
	if nrFailed > 0 {
		f.WriteString("<failure message=\"FAILED\"/>\n")
	}
	f.WriteString("</testcase></testsuite>")

	return nil
}

func chmodR(path, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}

	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err == nil {
			err = os.Chown(name, uid, gid)
		}
		return err
	})
}

func (j *Jenkins) setOwner(path string) error {
	return chmodR(path, "jenkins")
}

func (j *Jenkins) OwnWorkspace() error {
	if !j.IsActive() {
		return nil
	}
	return j.setOwner(j.wsPath)
}
