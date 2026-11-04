package matroska

import (
	"bytes"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// isStyledSubtitle recognises the ASS and SSA codec IDs, new and old. A prefix match
// again, as in codecPrivate.
func isStyledSubtitle(codec string) bool {
	for _, id := range []string{"S_TEXT/ASS", "S_TEXT/SSA", "S_ASS", "S_SSA"} {
		if strings.HasPrefix(codec, id) {
			return true
		}
	}
	return false
}

// maxSubtitleSetting is longer than any value the kept fields take.
const maxSubtitleSetting = 16

// keptSubtitleSections are the sections renderers read. Editors add others, such as
// Aegisub's project section, which holds the paths of the files it was working on,
// and [Fonts] and [Graphics], which hold embedded files.
var keptSubtitleSections = map[string]bool{
	"[script info]": true,
	"[v4 styles]":   true,
	"[v4+ styles]":  true,
	"[v4++ styles]": true,
	"[events]":      true,
}

// keptSubtitleInfo lists the [Script Info] fields that change how text is drawn. The
// rest are about the people and the tools that made the file.
var keptSubtitleInfo = map[string]bool{
	"scripttype":            true,
	"playresx":              true,
	"playresy":              true,
	"playdepth":             true,
	"layoutresx":            true,
	"layoutresy":            true,
	"wrapstyle":             true,
	"scaledborderandshadow": true,
	"collisions":            true,
	"timer":                 true,
	"ycbcr matrix":          true,
	"kerning":               true,
	"language":              true,
}

// subtitleField describes a field that gets a name of its own in the report.
type subtitleField struct {
	name     string
	identity bool // it names a person, or holds a file path that may do so
}

// subtitleFields lists what editors write. Field names are printed even by clean, which
// promises to print nothing from the file, so a key from the file is never used as one.
var subtitleFields = map[string]subtitleField{
	"title":                {"Subtitle title", false},
	"original script":      {"Subtitle original script", true},
	"original translation": {"Subtitle original translation", true},
	"original editing":     {"Subtitle original editing", true},
	"original timing":      {"Subtitle original timing", true},
	"script updated by":    {"Subtitle updated by", true},
	"update details":       {"Subtitle update details", false},
	"synch point":          {"Subtitle synch point", false},
	"audio file":           {"Subtitle audio file", true},
	"video file":           {"Subtitle video file", true},
	"timecodes file":       {"Subtitle timecodes file", true},
	"keyframes file":       {"Subtitle keyframes file", true},
}

// styledSubtitleHeader blanks every line of an ASS or SSA header that a renderer does
// not read. Authors name themselves here, and their tools add file paths.
//
// The header's length cannot change, and renderers read it line by line and stop at a
// NUL, so a removed line becomes newlines: zeros would hide what follows it.
//
// A header that cannot be read this way stays, with a warning that says so.
func (p *plan) styledSubtitleHeader(e element, where string) error {
	const cannotClean = "A styled subtitle header could not be cleaned, because it is compressed, very large or not plain text. It stays in the file, and such headers can name their authors."
	if p.trackPrivateEncoded || e.size > maxCodecHeaderRead {
		p.warn(warnSubtitleHeader, cannotClean)
		return nil
	}
	raw := p.r.peek(e, maxCodecHeaderRead)
	text := bytes.TrimRight(raw, "\x00")
	if bytes.IndexByte(text, 0) >= 0 {
		p.warn(warnSubtitleHeader, cannotClean)
		return nil
	}
	section := ""
	for pos := 0; pos < len(text); {
		end := pos
		for end < len(text) && text[end] != '\n' && text[end] != '\r' {
			end++
		}
		if end > pos {
			line := string(text[pos:end])
			if pos == 0 {
				line = strings.TrimPrefix(line, "\xEF\xBB\xBF")
			}
			var keep bool
			keep, section = p.judgeSubtitleLine(strings.TrimSpace(line), section, where)
			if !keep {
				// A removed line that starts the file takes its byte order mark with it,
				// which only makes the header plain ASCII.
				blank := bytes.Repeat([]byte{'\n'}, end-pos)
				if err := p.patch(core.Patch{Off: e.dataOff + int64(pos), Len: int64(end - pos), Data: blank}); err != nil {
					return err
				}
			}
		}
		pos = end + 1
	}
	return nil
}

// judgeSubtitleLine decides whether one trimmed line stays, and returns the section
// that the lines after it belong to.
func (p *plan) judgeSubtitleLine(line, section, where string) (keep bool, next string) {
	if line == "" {
		return true, section
	}
	if strings.HasPrefix(line, "[") {
		name := strings.ToLower(line)
		if keptSubtitleSections[name] {
			return true, name
		}
		p.add(field{"Subtitle section", core.CatHidden, core.RiskMed, decodeText}, line, where)
		return false, name
	}
	key, value, hasKey := strings.Cut(line, ":")
	key = strings.TrimSpace(key)
	lower := strings.ToLower(key)
	if !keptSubtitleSections[section] {
		// Embedded fonts run to thousands of lines of no interest. Paths are the point.
		if f, ok := subtitleFields[lower]; ok && f.identity {
			p.addSubtitleField(key, value, where)
		}
		return false, section
	}
	switch {
	case strings.HasPrefix(line, ";") || strings.HasPrefix(line, "!:"):
		p.add(field{"Subtitle comment", core.CatContent, core.RiskMed, decodeText}, line, where)
		return false, section
	case !hasKey:
		p.add(field{"Subtitle header text", core.CatContent, core.RiskLow, decodeText}, line, where)
		return false, section
	case section == "[script info]":
		if keptSubtitleInfo[lower] && isSubtitleSetting(value) {
			return true, section
		}
	case section == "[events]":
		if lower == "format" || lower == "dialogue" {
			return true, section
		}
	default:
		if lower == "format" || lower == "style" {
			return true, section
		}
	}
	p.addSubtitleField(key, value, where)
	return false, section
}

// isSubtitleSetting checks a kept field holds a setting, such as 1920, yes or TV.709,
// and not a sentence.
func isSubtitleSetting(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) <= maxSubtitleSetting && !strings.ContainsAny(value, " \t")
}

func (p *plan) addSubtitleField(key, value, where string) {
	value = strings.TrimSpace(value)
	f, ok := subtitleFields[strings.ToLower(key)]
	switch {
	case !ok:
		p.add(field{"Subtitle header field", core.CatContent, core.RiskLow, decodeText}, key+": "+value, where)
	case f.identity:
		p.add(field{f.name, core.CatIdentity, core.RiskHigh, decodeText}, value, where)
	default:
		p.add(field{f.name, core.CatContent, core.RiskLow, decodeText}, value, where)
	}
}
