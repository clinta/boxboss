package file

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

type slowReader struct {
	r io.Reader
}

func (s *slowReader) Read(p []byte) (n int, err error) {
	l := len(p)
	if l > 2048 {
		l = l / 2
	}
	return s.r.Read(p[:l])
}

func TestCmpReaders(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	b := make([]byte, chunkSize*4+5)
	_, err := rand.Read(b)
	assert.Nil(err)

	r1 := bytes.NewReader(b)
	r2 := bytes.NewReader(bytes.Clone(b))
	c, err := readersEqual(ctx, r1, r2)
	assert.True(c)
	assert.Nil(err)

	r1 = bytes.NewReader(append(b, 255))
	r2 = bytes.NewReader(b)
	c, err = readersEqual(ctx, r1, r2)
	assert.False(c)
	assert.Nil(err)

	r1 = bytes.NewReader(b)
	r2 = bytes.NewReader(append(b, 255))
	c, err = readersEqual(ctx, r1, r2)
	assert.False(c)
	assert.Nil(err)

	b2 := bytes.Clone(b)
	b2[chunkSize+5] += 1
	r1 = bytes.NewReader(b)
	r2 = bytes.NewReader(b2)
	c, err = readersEqual(ctx, r1, r2)
	assert.False(c)
	assert.Nil(err)
}

func TestCmpReadersSlow(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	b := make([]byte, chunkSize*4+5)
	_, err := rand.Read(b)
	assert.Nil(err)

	{
		r1 := &slowReader{bytes.NewReader(b)}
		r2 := bytes.NewReader(bytes.Clone(b))
		c, err := readersEqual(ctx, r1, r2)
		assert.True(c)
		assert.Nil(err)
	}

	{
		r1 := bytes.NewReader(b)
		r2 := &slowReader{bytes.NewReader(bytes.Clone(b))}
		c, err := readersEqual(ctx, r1, r2)
		assert.True(c)
		assert.Nil(err)
	}
}
