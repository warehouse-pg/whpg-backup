package iohelper

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/greenplum-db/gpbackup/gplog"
	"github.com/greenplum-db/gpbackup/operating"
)

func OpenFileForReading(filename string) (operating.ReadCloserAt, error) {
	fileHandle, err := operating.System.OpenFileRead(filename, os.O_RDONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("Unable to open file for reading: %s", err)
	}
	return fileHandle, nil
}

func MustOpenFileForReading(filename string) operating.ReadCloserAt {
	fileHandle, err := OpenFileForReading(filename)
	gplog.FatalOnError(err)
	return fileHandle
}

func OpenFileForWriting(filename string) (io.WriteCloser, error) {
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	fileHandle, err := operating.System.OpenFileWrite(filename, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("Unable to create or open file for writing: %s", err)
	}
	return fileHandle, nil
}

func MustOpenFileForWriting(filename string) io.WriteCloser {
	fileHandle, err := OpenFileForWriting(filename)
	gplog.FatalOnError(err)
	return fileHandle
}

func FileExistsAndIsReadable(filename string) bool {
	_, err := operating.System.Stat(filename)
	if err == nil {
		var fileHandle io.ReadCloser
		fileHandle, err = OpenFileForReading(filename)
		if fileHandle != nil {
			_ = fileHandle.Close()
		}
		if err == nil {
			return true
		}
	}
	return false
}

func ReadLinesFromFile(filename string) ([]string, error) {
	fileHandle, err := OpenFileForReading(filename)
	if err != nil {
		return nil, err
	}
	contents := make([]string, 0)
	scanner := bufio.NewScanner(fileHandle)
	for scanner.Scan() {
		contents = append(contents, scanner.Text())
	}
	err = fileHandle.Close()
	if err != nil {
		return nil, err
	}

	return contents, nil
}

func MustReadLinesFromFile(filename string) []string {
	contents, err := ReadLinesFromFile(filename)
	gplog.FatalOnError(err)
	return contents
}
