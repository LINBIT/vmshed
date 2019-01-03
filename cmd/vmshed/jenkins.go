package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
)

func isJenkins() bool { return *jenkins != "" }

func jenkinsSubDir(subdir string) (string, error) {
	if !isJenkins() {
		return "", errors.New("This is not a jenkins run")
	}
	p := filepath.Join(*jenkins, subdir)

	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}

	return p, os.MkdirAll(p, 0755)
}

func jenkinsLog(test, name string, buf *bytes.Buffer) error {
	p, err := jenkinsSubDir(test)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filepath.Join(p, name), buf.Bytes(), 0644)
}

func jenkinsXMLLog(restultsDir, name string, testRes *testResult, buf *bytes.Buffer) error {
	re, err := regexp.Compile("[^\t\n\r\x20-\x7e]")
	if err != nil {
		return err
	}

	p, err := jenkinsSubDir(restultsDir)

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

func jenkinsSetOwner(path string) error {
	jUser, err := user.Lookup("jenkins")
	if err != nil {
		return err
	}

	uid, err := strconv.Atoi(jUser.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(jUser.Gid)
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
