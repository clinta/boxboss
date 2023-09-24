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
func cmpReaders(r1 io.Reader, r2 io.Reader) (bool, error) {
	b1 := make([]byte, chunkSize)
	b2 := make([]byte, chunkSize)

	for {
		r1Len, r1Err := io.ReadAtLeast(r1, b1, chunkSize)
		r2Len, r2Err := io.ReadAtLeast(r2, b2, chunkSize)
		if r1Len == 0 && r2Len == 0 && errors.Is(r1Err, io.EOF) && errors.Is(r2Err, io.EOF) {
			return true, nil
		}
		if errors.Is(r1Err, io.EOF) || errors.Is(r2Err, io.EOF) {
			return false, nil
		}
		if r1Err != nil && !errors.Is(r1Err, io.ErrUnexpectedEOF) {
			return false, r1Err
		}
		if r2Err != nil && !errors.Is(r2Err, io.ErrUnexpectedEOF) {
			return false, r2Err
		}
		if r1Len != r2Len {
			return false, nil
		}
		for i := 0; i < r1Len; i++ {
			if b1[i] != b2[i] {
				return false, nil
			}
		}
	}
}
