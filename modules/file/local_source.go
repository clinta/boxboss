package file

import (
	"io"
	"os"
)

func LocalSource(path string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return os.Open(path) }
}
