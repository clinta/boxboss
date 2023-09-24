package file

import (
	"context"
	"errors"
	"io"
)

type File struct {
	path     string
	contents func() io.ReadSeekCloser
}

func (f *File) Name() string {
	return "file: " + f.path
}

func (f *File) Check(ctx context.Context) (bool, error) {
	// write out file to /tmp?
	// no that seems dumb, compare the buffers
	// if it's an http download or something, then write out to tmp
	// but for the base case just use the buffer as intended
	return false, nil
}

const chunkSize = 64000

// cmpReaders compares two readers and returns true if they are the same
func cmpReaders(r io.Reader, t io.Reader) (bool, error) {
	rBuf := make([]byte, chunkSize)
	tBuf := make([]byte, chunkSize)

	for {
		readFromR, errR := r.Read(rBuf)
		if errR != nil && !errors.Is(errR, io.EOF) {
			return false, errR
		}

		readFromT := 0
		tCmpBuf := tBuf[:readFromR]

		if readFromR == 0 && errors.Is(errR, io.EOF) {
			readFromT, errT := t.Read(tBuf[:1])
			if readFromT == 0 && errors.Is(errT, io.EOF) {
				return true, nil
			} else {
				return false, errT
			}
		}

		for readFromR > readFromT {
			nextReadFromT, errT := t.Read(tCmpBuf[readFromT:])
			if errT != nil && !errors.Is(errT, io.EOF) {
				return false, errT
			}
			prevReadFromT := readFromT
			readFromT = prevReadFromT + nextReadFromT
			rCmpBuf := rBuf[prevReadFromT:readFromT]
			for i, v2 := range tCmpBuf[prevReadFromT:readFromT] {
				if rCmpBuf[i] != v2 {
					return false, nil
				}
			}
			if errors.Is(errR, io.EOF) && errors.Is(errT, io.EOF) {
				return true, nil
			}
			if errors.Is(errR, io.EOF) || errors.Is(errT, io.EOF) {
				return false, nil
			}
		}
	}
}
