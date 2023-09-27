package file

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/unix"
)

const DefaultBackupLocation = "/var/lib/bossbox/file_backups"

type File struct {
	path           string
	contents       func() io.ReadCloser
	backupLocation string
	tmpFile        chan *tmpFileRes
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

func NewFile(path string, contents func() io.ReadCloser) File {
	f := File{
		path:           path,
		contents:       contents,
		backupLocation: "",
		tmpFile:        make(chan *tmpFileRes),
	}
	close(f.tmpFile)
	return f
}

func (f *File) Name() string {
	return "file: " + f.path
}

type tmpFileRes struct {
	name string
	err  error
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
	/*
		dstStat, err := os.Stat(f.path)
		if errors.Is(err, os.ErrNotExist) {
			// TODO: Create the file
			err = nil
		}
		if err != nil {
			return true, err
		}

		var UID int
		var GID int
		if stat, ok := dstStat.Sys().(*syscall.Stat_t); ok {
			UID = int(stat.Uid)
			GID = int(stat.Gid)
		}
	*/

	dst, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		dst, err = os.Create(f.path)
	}
	if err != nil {
		return true, err
	}
	defer dst.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(dst.Name()), filepath.Base(dst.Name()))
	if err != nil {
		return false, err
	}

	tmpFileName := tmpFile.Name()
	tmpFileClosed := make(chan struct{})

	context.AfterFunc(ctx, func() {
		// Clean up any lingering temp file after everything is done
		<-tmpFileClosed
		err := os.Remove(tmpFileName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// TODO logging is a mess
			log.Err(err).Msg("error removing temp file")
		}
	})

	contents := f.contents()
	reader := io.TeeReader(contents, tmpFile)

	eq, err := readersEqual(ctx, reader, dst)

	tmpFileWriteCtx, tmpFileWriteCancel := context.WithCancel(ctx)
	f.tmpFile = make(chan *tmpFileRes)

	// Continue writing the rest of the reader to the tmpfile
	// Then return the tmpfile name on the channel for future uses
	go func() {
		_, err := io.Copy(tmpFile, contents)
		tmpFileWriteCancel()
		<-tmpFileClosed
		r := &tmpFileRes{tmpFileName, err}
		for {
			select {
			case f.tmpFile <- r:
			case <-ctx.Done():
				close(f.tmpFile)
				return
			}
		}
	}()

	// Stop the copy early if the context is canceled
	go func() {
		<-tmpFileWriteCtx.Done()
		tmpFile.Close()
		close(tmpFileClosed)
	}()

	return !eq, err
}

// getTmpFile returns the path of the temporary file.
// During the Check() step the contents of the managed file is written out to getTmpFile.
// During the Apply() step the temporary file and the destination file are atomically swapped.
//
// Calling getTmpFile in a post-check hook will allow access to what will become the new file during the swap.
// Calling getTmpFile in an post-run hook will allow access to the previous file, for backup ect...
func (f *File) getTmpFile(ctx context.Context) (string, error) {
	var tmpFile *tmpFileRes
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case tmpFile = <-f.tmpFile:
	}
	return tmpFile.name, tmpFile.err
}

func (f *File) Apply(ctx context.Context) (bool, error) {
	dstStat, err := os.Stat(f.path)
	if err != nil {
		return false, err
	}

	tmpFileName, err := f.getTmpFile(ctx)
	if err != nil {
		return false, err
	}

	if stat, ok := dstStat.Sys().(*syscall.Stat_t); ok {
		if err = os.Chown(tmpFileName, int(stat.Uid), int(stat.Gid)); err != nil {
			return false, err
		}
	}
	if err = os.Chmod(tmpFileName, dstStat.Mode().Perm()); err != nil {
		return false, err
	}

	// TODO: Does this work? Open on a directory to get the fd?
	tmpD, err := os.Open(filepath.Dir(tmpFileName))
	if err != nil {
		return false, err
	}
	defer tmpD.Close()

	dstD, err := os.Open(filepath.Dir(f.path))
	if err != nil {
		return false, err
	}
	defer dstD.Close()

	// Renameat2 with RENAME_EXCHANGE atomically swaps the files, available since Linux 3.14
	err = unix.Renameat2(int(tmpD.Fd()), filepath.Base(tmpFileName), int(dstD.Fd()), filepath.Base(f.path), unix.RENAME_EXCHANGE)
	if err != nil {
		return false, errors.Join(ErrSwappingFiles, err)
	}

	if f.backupLocation != "" {
		if err := f.moveBackupFile(f.path, tmpFileName); err != nil {
			return true, errors.Join(ErrBackingUpFile, err)
		}
	}

	return true, nil
}

var ErrSwappingFiles = errors.New("failed to swap tmp and dst files")
var ErrBackingUpFile = errors.New("failed to backup old file")

func (f *File) moveBackupFile(dstFileName string, oldFileName string) error {
	dstDir := filepath.Dir(dstFileName)
	dstDirStat, err := os.Stat(dstDir)
	if err != nil {
		return err
	}

	backupDir := filepath.Join(f.backupLocation, dstDir)
	err = os.MkdirAll(backupDir, dstDirStat.Mode().Perm())
	if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return err
	}

	// Set correct mode and owner for each level of the backup path
	dstDirElems := filepath.SplitList(dstDir)
	for i := range dstDirElems {
		dir := filepath.Join(dstDirElems[:i]...)
		bakDir := filepath.Join(f.backupLocation, dir)
		stat, err := os.Stat(dir)
		if err != nil {
			return err
		}
		err = os.Chmod(bakDir, stat.Mode().Perm())
		if err != nil {
			return err
		}
		if stat, ok := stat.Sys().(*syscall.Stat_t); ok {
			if err = os.Chown(bakDir, int(stat.Uid), int(stat.Gid)); err != nil {
				return err
			}
		}
	}
	bakFileName := filepath.Join(f.backupLocation, dstFileName) + "." + strconv.FormatInt(time.Now().Unix(), 10)
	return os.Rename(oldFileName, bakFileName)
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

func (f *File) EnableBackups() error {
	if f.backupLocation == "" {
		return f.SetBackupLocation(DefaultBackupLocation)
	}
	return nil
}

func (f *File) SetBackupLocation(location string) error {
	f.backupLocation = location
	err := os.MkdirAll(f.backupLocation, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
