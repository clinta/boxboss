package file

import (
	"bytes"
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
	// compare the buffers, while writing out to a temp file
	// write out file to /tmp?
	// no that seems dumb, compare the buffers
	// if it's an http download or something, then write out to tmp
	// but for the base case just use the buffer as intended
	return false, nil
}

const chunkSize = 64000

// readersEqual compares two readers and returns true if they are the same
func readersEqual(ctx context.Context, r io.Reader, t io.Reader) (bool, error) {
	rBuf := make([]byte, chunkSize)
	tBuf := make([]byte, chunkSize)

	for {
		if err := checkCtx(ctx); err != nil {
			return false, err
		}
		readFromR, errR := r.Read(rBuf)
		if errR != nil && !errors.Is(errR, io.EOF) {
			return false, errR
		}

		readFromT := 0
		tCmpBuf := tBuf[:readFromR]

		if readFromR == 0 && errors.Is(errR, io.EOF) {
			if err := checkCtx(ctx); err != nil {
				return false, err
			}
			readFromT, errT := t.Read(tBuf[:1])
			if readFromT == 0 && errors.Is(errT, io.EOF) {
				return true, nil
			} else {
				return false, errT
			}
		}

		for readFromR > readFromT {
			if err := checkCtx(ctx); err != nil {
				return false, err
			}
			nextReadFromT, errT := t.Read(tCmpBuf[readFromT:])
			if errT != nil && !errors.Is(errT, io.EOF) {
				return false, errT
			}
			prevReadFromT := readFromT
			readFromT = prevReadFromT + nextReadFromT
			if !bytes.Equal(rBuf[prevReadFromT:readFromT], tCmpBuf[prevReadFromT:readFromT]) {
				return false, nil
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

func checkCtx(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
