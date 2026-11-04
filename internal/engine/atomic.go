package engine

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const (
	tempPrefix = ".hideme-"
	tempSuffix = ".tmp"
	// outputBuffer batches a handler's small writes. Large copies pass straight
	// through, since bufio does not double-buffer a write bigger than this.
	outputBuffer = 1 << 20
)

func isTempName(name string) bool {
	return strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, tempSuffix)
}

// tempOutput is the file a handler writes into. It is mode 0600 until verified,
// because at that point it might still hold whatever the handler failed to remove.
type tempOutput struct {
	file      *os.File
	name      string
	buf       *bufio.Writer
	size      int64
	closed    bool
	published bool
}

func newTempOutput(dir string) (*tempOutput, error) {
	f, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return nil, err
	}
	return &tempOutput{file: f, name: f.Name(), buf: bufio.NewWriterSize(f, outputBuffer)}, nil
}

func (t *tempOutput) Write(p []byte) (int, error) {
	n, err := t.buf.Write(p)
	t.size += int64(n)
	return n, err
}

// flushAndSync gets the bytes onto the disk before they are verified, so that what
// is checked is what stays. A rename that outlives a crash must not expose a file
// whose contents did not.
func (t *tempOutput) flushAndSync() error {
	if err := t.buf.Flush(); err != nil {
		return err
	}
	return t.file.Sync()
}

func (t *tempOutput) close() error {
	t.closed = true
	return t.file.Close()
}

// discard removes the temp file unless it has become the output. It is safe to
// call at any point, and it is what keeps failures from leaving files behind.
func (t *tempOutput) discard() {
	if t.published {
		return
	}
	if !t.closed {
		t.file.Close()
		t.closed = true
	}
	os.Remove(t.name)
}

// publish gives the verified file its real name. Without replace it must never
// overwrite anything, even if another process created the output after the early
// check. O_EXCL claims the name atomically, and this works on every file system,
// including FAT and SMB shares where hard links do not.
func publish(temp, output string, replace bool) error {
	if replace {
		return rename(temp, output)
	}
	claim, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errOutputExists
		}
		return err
	}
	if err := claim.Close(); err != nil {
		os.Remove(output)
		return err
	}
	if err := rename(temp, output); err != nil {
		os.Remove(output)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// syncDir makes the rename itself durable, so that "--in-place" cannot appear to
// succeed and then revert after a power cut. Directories cannot be synced on
// Windows, where the file system journals renames anyway, so failure is ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
