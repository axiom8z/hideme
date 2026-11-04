package engine

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
)

// zoneNames labels the URLZONE values Windows writes.
var zoneNames = map[string]string{
	"0": "local machine",
	"1": "local network",
	"2": "trusted sites",
	"3": "internet",
	"4": "restricted sites",
}

// zoneFindings decodes a Zone.Identifier stream, an INI-style text that Windows and
// browsers attach to downloaded files and which can hold the URL they came from. It
// is pure parsing with no system calls, so its tests run on every platform.
func zoneFindings(data []byte, where string) []core.Finding {
	finding := func(risk core.Risk, field, value string) core.Finding {
		return core.Finding{Category: core.CatFilesystem, Risk: risk, Field: field, Value: value, Where: where, Action: core.ActionRemove}
	}
	var out []core.Finding
	for line := range strings.SplitSeq(decodeZoneText(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue // the [ZoneTransfer] section header
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "hosturl":
			out = append(out, finding(core.RiskHigh, "Download URL", value))
		case "referrerurl":
			out = append(out, finding(core.RiskHigh, "Download referrer URL", value))
		case "zoneid":
			label := value
			if name, known := zoneNames[strings.TrimSpace(value)]; known {
				label = fmt.Sprintf("%s (%s)", value, name)
			}
			out = append(out, finding(core.RiskLow, "Download zone", label))
		default:
			out = append(out, finding(core.RiskMed, "Download mark field", line))
		}
	}
	if len(out) == 0 {
		return []core.Finding{finding(core.RiskLow, "Download mark", "")}
	}
	return out
}

// decodeZoneText handles the UTF-16 that some tools write in place of ANSI.
func decodeZoneText(data []byte) string {
	var swap bool
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		swap = true
	default:
		return string(data)
	}
	units := make([]uint16, 0, len(data)/2)
	for i := 2; i+1 < len(data); i += 2 {
		lo, hi := data[i], data[i+1]
		if swap {
			lo, hi = hi, lo
		}
		units = append(units, uint16(hi)<<8|uint16(lo))
	}
	return string(utf16.Decode(units))
}
