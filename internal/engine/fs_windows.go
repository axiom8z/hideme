package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/axiom8z/hideme/internal/core"
)

// FilesystemLimit states what inspect can and cannot see on this platform.
const FilesystemLimit = "NTFS alternate data streams are reported, including the download URL in Zone.Identifier. Outputs are new files and inherit none of them; the original keeps its streams unless replaced with --in-place."

const (
	zoneStream = "Zone.Identifier"
	// maxZoneBytes bounds what is read of a Zone.Identifier stream. Real ones are a
	// few hundred bytes, and a stream can be as large as any file.
	maxZoneBytes = 64 * 1024
	// maxStreams bounds the report for a file with absurdly many streams.
	maxStreams = 256

	errorHandleEOF       = syscall.Errno(38)
	errorInvalidFunction = syscall.Errno(1)
	errorInvalidParam    = syscall.Errno(87)
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procFindFirstStream = kernel32.NewProc("FindFirstStreamW")
	procFindNextStream  = kernel32.NewProc("FindNextStreamW")

	advapi32                         = syscall.NewLazyDLL("advapi32.dll")
	procGetNamedSecurityInfo         = advapi32.NewProc("GetNamedSecurityInfoW")
	procSetNamedSecurityInfo         = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetSecurityDescriptorControl = advapi32.NewProc("GetSecurityDescriptorControl")
)

// findStreamData is WIN32_FIND_STREAM_DATA: a 64-bit size and a name of MAX_PATH+36
// UTF-16 units.
type findStreamData struct {
	size int64
	name [296]uint16
}

type dataStream struct {
	name string
	size int64
}

// listStreams returns the named streams of a file, leaving out the unnamed main one.
func listStreams(path string) ([]dataStream, error) {
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, err
	}
	if err := procFindFirstStream.Find(); err != nil {
		return nil, err
	}
	var data findStreamData
	h, _, callErr := procFindFirstStream.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&data)), 0)
	if syscall.Handle(h) == syscall.InvalidHandle {
		// A file system without streams, such as FAT, reports one of these.
		if errors.Is(callErr, errorHandleEOF) || errors.Is(callErr, errorInvalidParam) || errors.Is(callErr, errorInvalidFunction) {
			return nil, nil
		}
		return nil, callErr
	}
	defer syscall.FindClose(syscall.Handle(h))

	var streams []dataStream
	for len(streams) < maxStreams {
		// The name reads ":name:$DATA", and "::$DATA" is the file's main content.
		full := syscall.UTF16ToString(data.name[:])
		if name, ok := strings.CutSuffix(strings.TrimPrefix(full, ":"), ":$DATA"); ok && name != "" {
			streams = append(streams, dataStream{name, data.size})
		}
		ok, _, nextErr := procFindNextStream.Call(h, uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if errors.Is(nextErr, errorHandleEOF) {
				return streams, nil
			}
			return streams, nextErr
		}
	}
	return streams, nil
}

// extendedPath lets the raw Win32 calls here reach the long paths that the os package
// handles by itself.
func extendedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || len(abs) < 248 || strings.HasPrefix(abs, `\\?\`) {
		return path
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

// inspectFilesystem reports the file's alternate data streams.
func inspectFilesystem(path string, rep *core.Report) {
	streams, err := listStreams(path)
	if err != nil {
		rep.Warn("could not list this file's alternate data streams: %s", describe(err))
	}
	if len(streams) == maxStreams {
		rep.Warn("this file has at least %d alternate data streams and only that many are listed; outputs inherit none of them", maxStreams)
	}
	for _, s := range streams {
		if strings.EqualFold(s.name, zoneStream) {
			reportZoneStream(path+":"+s.name, rep)
			continue
		}
		rep.Add(core.Finding{
			Category: core.CatFilesystem,
			Risk:     core.RiskMed,
			Field:    "Alternate data stream",
			Value:    fmt.Sprintf("%s, %d bytes", s.name, s.size),
			Where:    "NTFS stream", // the name is the user's own, so it stays in the value
			Action:   core.ActionRemove,
		})
	}
}

func reportZoneStream(streamPath string, rep *core.Report) {
	data, err := readZoneStream(streamPath)
	if err != nil {
		rep.Warn("could not read this file's Zone.Identifier stream: %s", describe(err))
		rep.Add(core.Finding{
			Category: core.CatFilesystem,
			Risk:     core.RiskHigh,
			Field:    "Download mark",
			Where:    "NTFS stream " + zoneStream,
			Action:   core.ActionRemove,
		})
		return
	}
	for _, f := range zoneFindings(data, "NTFS stream "+zoneStream) {
		rep.Add(f)
	}
}

func readZoneStream(streamPath string) ([]byte, error) {
	f, err := os.Open(streamPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxZoneBytes))
}

// hardLinkCount says how many names the open file has.
func hardLinkCount(f *os.File) (uint64, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

// sourceTimes returns the access and creation times the stat structure holds.
func sourceTimes(info os.FileInfo) (access, create time.Time) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return info.ModTime(), time.Time{}
	}
	return time.Unix(0, data.LastAccessTime.Nanoseconds()), time.Unix(0, data.CreationTime.Nanoseconds())
}

// setCreationTime goes through an opened handle because os.Chtimes has no way to
// reach the creation time. A file's creation time otherwise gives away when it was
// cleaned, or, through NTFS tunneling, when an earlier file of that name was made.
func setCreationTime(path string, t time.Time) error {
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(p, syscall.FILE_WRITE_ATTRIBUTES,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	created := syscall.NsecToFiletime(t.UnixNano())
	return syscall.SetFileTime(h, &created, nil, nil)
}

// rename retries briefly, because on Windows an indexer or virus scanner that has
// just noticed a new file can hold it open for a moment and make the rename fail with
// a sharing violation that has nothing to do with the outcome. Windows also refuses to
// replace a read-only file, though on other systems a rename over one simply works,
// since it changes the directory and not the file; so the attribute is lifted first,
// and put back if the file turns out not to be replaced after all.
func rename(from, to string) error {
	restore := liftReadOnly(to)
	const attempts = 10
	var err error
	for i := range attempts {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if !isTransientSharingError(err) {
			break
		}
		time.Sleep(time.Duration(i+1) * 25 * time.Millisecond)
	}
	restore()
	return err
}

func liftReadOnly(path string) (restore func()) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm()&0o200 != 0 {
		return func() {}
	}
	if err := os.Chmod(path, info.Mode().Perm()|0o200); err != nil {
		return func() {}
	}
	return func() { _ = os.Chmod(path, info.Mode().Perm()) }
}

func isTransientSharingError(err error) bool {
	const (
		errorAccessDenied     = syscall.Errno(5)
		errorSharingViolation = syscall.Errno(32)
		errorLockViolation    = syscall.Errno(33)
	)
	return errors.Is(err, errorAccessDenied) || errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation)
}

// unreachableReason says why unreachableName refuses a name.
const unreachableReason = "Windows paths cannot reach a name that ends in a dot or a space or holds an invalid character, so another file could be cleaned in its place; rename it first"

// unreachableName reports a name that exists on NTFS but that an ordinary path cannot
// address. Win32 strips a trailing dot or space, so "a.png." opens a.png, or nothing,
// and an unpaired surrogate reaches Go as U+FFFD, the name of some other file. A
// genuine U+FFFD in a name is refused along with them, since nothing tells them apart.
func unreachableName(name string) bool {
	return strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || strings.ContainsRune(name, utf8.RuneError)
}

// pathCannotReach is unreachableName for a path from the command line, or a wildcard's
// match, where any element can be at fault. There "." and ".." are normal, and a \\?\
// prefix turns Win32's rewriting off, so the path means what it says.
func pathCannotReach(path string) bool {
	if strings.HasPrefix(path, `\\?\`) {
		return false
	}
	elements := strings.FieldsFunc(path[len(filepath.VolumeName(path)):], func(r rune) bool { return r == '/' || r == '\\' })
	return slices.ContainsFunc(elements, func(name string) bool {
		return name != "." && name != ".." && unreachableName(name)
	})
}

// onDiskName returns path with its last element spelled the way the directory holds
// it. Renaming over a file gives it the name the rename was told, so replacing
// longna~1.jpg in place would throw the real long name away for good, and PHOTO.JPG
// would change the case of photo.jpg.
func onDiskName(path string) string {
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return path
	}
	var data syscall.Win32finddata
	h, err := syscall.FindFirstFile(p, &data)
	if err != nil {
		return path
	}
	defer syscall.FindClose(h)
	return filepath.Join(filepath.Dir(path), syscall.UTF16ToString(data.FileName[:]))
}

const (
	seFileObject = 1

	daclSecurityInformation            = 0x00000004
	protectedDACLSecurityInformation   = 0x80000000
	unprotectedDACLSecurityInformation = 0x20000000

	seDACLProtected = 0x1000
	inheritedACE    = 0x10
)

// aclHeader is the fixed start of a Windows ACL. The ACEs follow it.
type aclHeader struct {
	revision, sbz1    byte
	size, count, sbz2 uint16
}

// fileDACL is a file's permission list, in memory Windows allocated.
type fileDACL struct {
	descriptor unsafe.Pointer
	dacl       *aclHeader // nil for a file system without permissions
}

func readDACL(path string) (fileDACL, error) {
	var d fileDACL
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return d, err
	}
	if err := procGetNamedSecurityInfo.Find(); err != nil {
		return d, err
	}
	r, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInformation,
		0, 0, uintptr(unsafe.Pointer(&d.dacl)), 0, uintptr(unsafe.Pointer(&d.descriptor)))
	if r != 0 {
		return d, syscall.Errno(r)
	}
	return d, nil
}

func (d fileDACL) free() {
	if d.descriptor != nil {
		syscall.LocalFree(syscall.Handle(uintptr(d.descriptor)))
	}
}

// protected reports that the file ignores what its folder would give it.
func (d fileDACL) protected() bool {
	var control uint16
	var revision uint32
	r, _, _ := procGetSecurityDescriptorControl.Call(uintptr(d.descriptor), uintptr(unsafe.Pointer(&control)), uintptr(unsafe.Pointer(&revision)))
	return r != 0 && control&seDACLProtected != 0
}

func (d fileDACL) aceCount() int {
	if d.dacl == nil {
		return 0
	}
	return int(d.dacl.count)
}

// hasOwnEntries reports whether the file's permissions are its own rather than only
// what its folder hands down, which a new file gets anyway. It walks the entries by
// hand since each states its own size.
func (d fileDACL) hasOwnEntries() bool {
	if d.dacl == nil {
		return false
	}
	if d.protected() {
		return true
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(d.dacl)), d.dacl.size)
	off := int(unsafe.Sizeof(aclHeader{}))
	for range d.aceCount() {
		if off+4 > len(raw) {
			return true // not understood, so assume it matters
		}
		if raw[off+1]&inheritedACE == 0 {
			return true
		}
		size := int(raw[off+2]) | int(raw[off+3])<<8
		if size == 0 {
			return true
		}
		off += size
	}
	return false
}

func (d fileDACL) applyTo(path string) error {
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return err
	}
	var which uintptr = daclSecurityInformation | unprotectedDACLSecurityInformation
	if d.protected() {
		which = daclSecurityInformation | protectedDACLSecurityInformation
	}
	r, _, _ := procSetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject, which, 0, 0, uintptr(unsafe.Pointer(d.dacl)), 0)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// copyPermissions does what copying the mode bits does elsewhere. os.Chmod here only
// touches the read-only attribute, and a new file takes its permissions from its
// folder, so a private file in a shared folder would come out readable by everyone the
// folder lets read. A file that simply follows its folder needs nothing done, which also
// keeps writing to a FAT stick from failing on every file. Replacing a file also keeps it
// hidden if it was.
func copyPermissions(src, dst string, replacing bool) error {
	if replacing {
		keepHidden(src, dst)
	}
	d, err := readDACL(src)
	if err != nil {
		return err
	}
	defer d.free()
	if !d.hasOwnEntries() {
		return nil
	}
	return d.applyTo(dst)
}

func keepHidden(src, dst string) {
	from, err := syscall.UTF16PtrFromString(extendedPath(src))
	if err != nil {
		return
	}
	attrs, err := syscall.GetFileAttributes(from)
	if err != nil || attrs&syscall.FILE_ATTRIBUTE_HIDDEN == 0 {
		return
	}
	to, err := syscall.UTF16PtrFromString(extendedPath(dst))
	if err != nil {
		return
	}
	if now, err := syscall.GetFileAttributes(to); err == nil {
		_ = syscall.SetFileAttributes(to, now|syscall.FILE_ATTRIBUTE_HIDDEN)
	}
}
