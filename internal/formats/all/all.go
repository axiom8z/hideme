// Package all imports every format handler for its side effect of registering
// itself. Only the command imports this, which keeps handlers from depending on one
// another: each reaches its neighbours through the registry in internal/core.
package all

import (
	_ "github.com/axiom8z/hideme/internal/formats/aiff"
	_ "github.com/axiom8z/hideme/internal/formats/avi"
	_ "github.com/axiom8z/hideme/internal/formats/flac"
	_ "github.com/axiom8z/hideme/internal/formats/gif"
	_ "github.com/axiom8z/hideme/internal/formats/isobmff"
	_ "github.com/axiom8z/hideme/internal/formats/jpeg"
	_ "github.com/axiom8z/hideme/internal/formats/matroska"
	_ "github.com/axiom8z/hideme/internal/formats/mp3"
	_ "github.com/axiom8z/hideme/internal/formats/office"
	_ "github.com/axiom8z/hideme/internal/formats/ogg"
	_ "github.com/axiom8z/hideme/internal/formats/pdf"
	_ "github.com/axiom8z/hideme/internal/formats/png"
	_ "github.com/axiom8z/hideme/internal/formats/svg"
	_ "github.com/axiom8z/hideme/internal/formats/tiff"
	_ "github.com/axiom8z/hideme/internal/formats/wav"
	_ "github.com/axiom8z/hideme/internal/formats/webp"
)
