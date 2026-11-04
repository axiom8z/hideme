package engine

import (
	"os"
	"time"
)

// fileTimes is what to stamp on an output. A zero create or access means the file
// system's own value stands.
type fileTimes struct {
	set            bool // false leaves the times a fresh file has
	access, modify time.Time
	create         time.Time
	// replaceCreate is set when the creation time must be forced after the rename.
	// NTFS "tunnels" it: a file created under a name removed in the last 15 seconds
	// inherits the removed file's creation time. Deleting an output and cleaning again
	// would otherwise date the new file from the old one, whatever was set beforehand.
	replaceCreate bool
}

// times works out the stamps for an output of a source with the given info.
func (t Timestamp) times(source os.FileInfo) fileTimes {
	switch t {
	case TimestampZero:
		return fileTimes{set: true, access: ZeroTime, modify: ZeroTime, create: ZeroTime, replaceCreate: true}
	case TimestampKeep:
		access, create := sourceTimes(source)
		return fileTimes{set: true, access: access, modify: source.ModTime(), create: create, replaceCreate: !create.IsZero()}
	}
	return fileTimes{create: time.Now(), replaceCreate: true}
}

func (ft fileTimes) applyBeforePublish(path string) error {
	if !ft.set {
		return nil
	}
	if err := os.Chtimes(path, ft.access, ft.modify); err != nil {
		return err
	}
	return ft.applyCreate(path)
}

func (ft fileTimes) applyAfterPublish(path string) error {
	if !ft.replaceCreate {
		return nil
	}
	return ft.applyCreate(path)
}

func (ft fileTimes) applyCreate(path string) error {
	if ft.create.IsZero() {
		return nil
	}
	return setCreationTime(path, ft.create)
}
