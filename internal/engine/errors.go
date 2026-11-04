package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/axiom8z/hideme/internal/core"
)

// Errors the engine itself raises. Each maps to a stable Result.Kind.
var (
	errVerification = errors.New("verification failed, output discarded")
	errOutputExists = errors.New("the output already exists; use --force to replace it")
	errSameFile     = errors.New("the output would be the source file itself; use -o DIR, a different --suffix or --in-place")
	errCollision    = errors.New("two inputs would write to the same output")
	errInvalidInput = errors.New("this path cannot be processed")
	// errMayLoseContent is the reason in full, and describe gives it room to be.
	errMayLoseContent = errors.New("--in-place refused: the cleaned copy may lack content the original holds, such as frames stored where no tag refers to them. The original is untouched and no output was written. Write a copy instead (leave out --in-place, or use -o DIR) and check it, or pass --force to replace the original anyway")
	errPanic          = errors.New("internal error in a format handler; nothing was written")
)

// Result.Kind values, part of the documented JSON schema.
const (
	KindCanceled     = "canceled"
	KindVerification = "verification"
	KindUnsupported  = "unsupported"
	KindEncrypted    = "encrypted"
	KindTooLarge     = "too-large"
	KindMalformed    = "malformed"
	KindOutputExists = "output-exists"
	KindSameFile     = "same-file"
	KindCollision    = "output-collision"
	KindInvalidInput = "invalid-input"
	KindNotFound     = "not-found"
	KindInternal     = "internal"
	KindIO           = "io"
	KindSymlink      = "symlink"
	KindSpecial      = "special-file"
	KindHasSuffix    = "has-output-suffix"
	KindOutputDir    = "output-directory"
	KindTempFile     = "temp-file"
	// KindMayLoseContent means --in-place was refused to protect the original.
	KindMayLoseContent = "may-lose-content"
)

func kindOf(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return KindCanceled
	case errors.Is(err, errVerification):
		return KindVerification
	case errors.Is(err, errPanic):
		return KindInternal
	case errors.Is(err, core.ErrUnsupported):
		return KindUnsupported
	case errors.Is(err, core.ErrEncrypted):
		return KindEncrypted
	case errors.Is(err, core.ErrTooLarge):
		return KindTooLarge
	case errors.Is(err, core.ErrMalformed):
		return KindMalformed
	case errors.Is(err, errOutputExists):
		return KindOutputExists
	case errors.Is(err, errSameFile):
		return KindSameFile
	case errors.Is(err, errCollision):
		return KindCollision
	case errors.Is(err, errInvalidInput):
		return KindInvalidInput
	case errors.Is(err, errMayLoseContent):
		return KindMayLoseContent
	case errors.Is(err, fs.ErrNotExist):
		return KindNotFound
	}
	return KindIO
}

// fromHandler marks an error whose message may repeat text from the file, which is any
// error a handler returns: handlers explain a refusal by quoting what they found, such as
// a ZIP entry name or a declared encoding. The engine wraps every such error where it
// receives it, so what a handler writes in a message cannot decide how far it travels,
// and neither a new handler nor a missed quote can put a file's text in a log.
type fromHandler struct{ err error }

func (e fromHandler) Error() string { return e.err.Error() }
func (e fromHandler) Unwrap() error { return e.err }

// describe turns an error into a message for people. It may hold text from the engine
// and the operating system, and never from the file: a handler's error gets a fixed
// message for its kind, and detail keeps its own. File-system errors repeat the path,
// which the caller prints anyway, and core.SafeText would cut a long path short and
// lose the part of the message that explains the problem.
func describe(err error) string {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var handler fromHandler
	switch {
	case errors.As(err, &pathErr):
		return core.SafeText(fmt.Sprintf("%s: %v", pathErr.Op, pathErr.Err))
	case errors.As(err, &linkErr):
		return core.SafeText(fmt.Sprintf("%s: %v", linkErr.Op, linkErr.Err))
	case errors.As(err, &handler):
		return reasonForKind(kindOf(err))
	}
	return core.SafeMessage(err.Error())
}

// reasonForKind is what a person is told about a handler's error unless they ask for its
// own words.
func reasonForKind(kind string) string {
	switch kind {
	case KindCanceled:
		return context.Canceled.Error()
	case KindVerification:
		return errVerification.Error()
	case KindInternal:
		return errPanic.Error()
	case KindUnsupported:
		return core.ErrUnsupported.Error() + ": HideMe cannot clean this kind or variant of file"
	case KindEncrypted:
		return core.ErrEncrypted.Error() + ": its content cannot be rewritten"
	case KindTooLarge:
		return core.ErrTooLarge.Error()
	case KindMalformed:
		return core.ErrMalformed.Error() + ": it could not be parsed with enough confidence to guarantee its metadata was found"
	}
	return "the file could not be processed"
}

// detail returns a handler's own words about a failure, or nothing if err did not come
// from one.
func detail(err error) string {
	var handler fromHandler
	if !errors.As(err, &handler) {
		return ""
	}
	return core.SafeMessage(err.Error())
}

// failure is the Result for an input that was not processed. A handler refusing a
// file during a walk is a separate, milder status; see worker.process.
func failure(path string, err error) Result {
	return Result{Path: path, Status: StatusFailed, Reason: describe(err), Detail: detail(err), Kind: kindOf(err)}
}

func skipped(path, kind, reason string) Result {
	return Result{Path: path, Status: StatusSkipped, Reason: reason, Kind: kind}
}
