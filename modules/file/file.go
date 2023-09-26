package file

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
)

type File struct {
	path     string
	contents func() io.Reader
	// TODO: Possible temp file
	//   Should probably remove the temp file if ctx is canceled
	// TODO: File mode, or should mode be a separate module?
	//   Should copy the mode of the source to the destination
	// TODO: create directories? Or should this also be a separate module?

	// More ideas
	// have a sub-context that runs for the duration of a state run from pre-check to the last post-apply
	// we can hook into this context to delete temp files.
	// check can be writing out to a file, return a false quickly, but keep making the file in a goroutine (with the ctx)
	// when apply is run, it waits for the check writing process to finish, then moves the tmp file
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

	// Note to self: If the file changes between the check and the run, the trigger watching the file
	// should cancel the context and start over, so this shouldn't be something that needs to be handled here

	// If the source is http, the contents reader should be wrapped in a tee that outputs to a file during check
	// then that file should be used as the source during apply

	// If the source is a local file, this is not needed

	// TODO: What if the file doesn't exist?
	dst, err := os.Open(f.path)
	if err != nil {
		return true, err
	}
	defer dst.Close()
	eq, err := readersEqual(ctx, f.contents(), dst)
	return !eq, err
}

func (f *File) Apply(ctx context.Context) (bool, error) {
	// For performance reasons, we will assume changes are always true
	// We could be more accurate by teeing through readersEqual, but this is unnecessary overhead,
	// Check already indicated changes are required, so assume they are being made

	w, err := func() (int64, error) {
		dst, err := os.Open(f.path)
		if err != nil {
			return 0, err
		}
		defer dst.Close()
		return io.Copy(dst, f.contents())
	}()
	if w > 0 {
		os.Truncate(f.path, w)
	}
	return true, err
}

const chunkSize = 65_536

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
