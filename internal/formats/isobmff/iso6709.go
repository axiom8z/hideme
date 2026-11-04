package isobmff

import (
	"fmt"
	"strconv"
	"strings"
)

// parseISO6709 reads a location string such as "+48.8584+002.2945+035.000/", the
// form QuickTime and Android use. Degrees may also be written as DDMM.MMMM or
// DDMMSS.SS, which ISO 6709 allows and some cameras use.
func parseISO6709(s string) (lat, lon, alt float64, hasAlt, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "/C"); i >= 0 {
		s = s[:i] // a coordinate reference system may follow as "CRSxxx"
	}
	fields := splitSigned(s)
	if len(fields) < 2 || len(fields) > 3 {
		return 0, 0, 0, false, false
	}
	lat, latOK := parseAngle(fields[0], 2)
	lon, lonOK := parseAngle(fields[1], 3)
	if !latOK || !lonOK || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, 0, false, false
	}
	if len(fields) == 3 {
		value, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return 0, 0, 0, false, false
		}
		alt, hasAlt = value, true
	}
	return lat, lon, alt, hasAlt, true
}

// splitSigned cuts s in front of each sign. It returns nil unless s begins with one.
func splitSigned(s string) []string {
	if s == "" || (s[0] != '+' && s[0] != '-') {
		return nil
	}
	var fields []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || s[i] == '+' || s[i] == '-' {
			fields = append(fields, s[start:i])
			start = i
		}
	}
	return fields
}

// parseAngle reads a signed angle whose whole part has degreeDigits digits for
// plain degrees, two more with minutes, or four more with seconds.
func parseAngle(field string, degreeDigits int) (float64, bool) {
	sign := 1.0
	if field[0] == '-' {
		sign = -1
	}
	digits := field[1:]
	whole := digits
	if i := strings.IndexByte(digits, '.'); i >= 0 {
		whole = digits[:i]
	}
	value, err := strconv.ParseFloat(digits, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	switch len(whole) {
	case degreeDigits:
		return sign * value, true
	case degreeDigits + 2:
		degrees := float64(int(value / 100))
		return sign * (degrees + (value-degrees*100)/60), true
	case degreeDigits + 4:
		degrees := float64(int(value / 10000))
		rest := value - degrees*10000
		minutes := float64(int(rest / 100))
		return sign * (degrees + minutes/60 + (rest-minutes*100)/3600), true
	}
	return 0, false
}

// formatCoordinates matches how the exif and xmp packages show a location.
func formatCoordinates(lat, lon float64) string {
	return fmt.Sprintf("%.6f, %.6f", lat, lon)
}
