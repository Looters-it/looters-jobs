// Package geo reads uploaded KML and GeoJSON files into one internal form.
// It only parses and validates: every geometric computation (validity,
// simplification, adjacency) is left to PostGIS.
package geo

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Unit is one municipality in the internal form shared by every input format.
type Unit struct {
	Name string
	Code string // official code when the file carries one, otherwise empty
	// Geometry is a GeoJSON MultiPolygon in WGS84 longitude/latitude.
	Geometry json.RawMessage
}

// Problem codes, translated by the web interface.
const (
	ProblemMissingName    = "MISSING_NAME"
	ProblemNoPolygon      = "NO_POLYGON"
	ProblemBadCoordinates = "BAD_COORDINATES"
	ProblemRingTooShort   = "RING_TOO_SHORT"
	ProblemDuplicateCode  = "DUPLICATE_CODE"
)

// Problem is a validation error on one feature of the file.
type Problem struct {
	Feature int    `json:"feature"` // 1-based position in the file
	Name    string `json:"name,omitempty"`
	Code    string `json:"code"`
}

// FileError means the file as a whole cannot be read as the announced format.
type FileError struct {
	Code   string // INVALID_FILE or NOT_FEATURE_COLLECTION
	Detail string
}

func (e *FileError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Detail) }

// Format names, as stored in GeoUpload.format.
const (
	FormatGeoJSON = "GEOJSON"
	FormatKML     = "KML"
)

// Parse reads a file of the given format. It returns the valid units and
// the problems found; a *FileError when the file cannot be read at all.
func Parse(format string, r io.Reader) ([]Unit, []Problem, error) {
	var c collector
	var err error
	switch format {
	case FormatGeoJSON:
		err = parseGeoJSON(r, &c)
	case FormatKML:
		err = parseKML(r, &c)
	default:
		err = &FileError{Code: "INVALID_FILE", Detail: "unknown format " + format}
	}
	if err != nil {
		return nil, nil, err
	}
	return c.units, c.problems, nil
}

// A ring is a closed line of [longitude, latitude] positions; a polygon is
// an outer ring followed by its holes.
type (
	ring    = [][2]float64
	polygon = []ring
)

// collector validates features as the parsers find them.
type collector struct {
	units    []Unit
	problems []Problem
	seen     int
	codes    map[string]bool
}

// reject records a feature that the parser itself could not read.
func (c *collector) reject(name, problem string) {
	c.seen++
	c.problems = append(c.problems, Problem{Feature: c.seen, Name: strings.TrimSpace(name), Code: problem})
}

func (c *collector) add(name, code string, polygons []polygon) {
	c.seen++
	name, code = strings.TrimSpace(name), strings.TrimSpace(code)
	fail := func(problem string) {
		c.problems = append(c.problems, Problem{Feature: c.seen, Name: name, Code: problem})
	}

	if name == "" {
		fail(ProblemMissingName)
		return
	}
	if len(polygons) == 0 {
		fail(ProblemNoPolygon)
		return
	}
	for _, poly := range polygons {
		if len(poly) == 0 {
			fail(ProblemNoPolygon)
			return
		}
		for _, r := range poly {
			if len(r) < 4 {
				fail(ProblemRingTooShort)
				return
			}
			for _, p := range r {
				// Outside these bounds the file is not in WGS84 degrees
				// (for example it is in a projected system, in metres).
				if p[0] < -180 || p[0] > 180 || p[1] < -90 || p[1] > 90 {
					fail(ProblemBadCoordinates)
					return
				}
			}
		}
	}
	if code != "" {
		if c.codes == nil {
			c.codes = map[string]bool{}
		}
		if c.codes[code] {
			fail(ProblemDuplicateCode)
			return
		}
		c.codes[code] = true
	}

	geometry, err := json.Marshal(struct {
		Type        string    `json:"type"`
		Coordinates []polygon `json:"coordinates"`
	}{"MultiPolygon", polygons})
	if err != nil {
		fail(ProblemBadCoordinates)
		return
	}
	c.units = append(c.units, Unit{Name: name, Code: code, Geometry: geometry})
}

// Property names tried, in order and ignoring case, for the name and the
// official code of a municipality.
var (
	nameKeys = []string{"name", "nome", "comune", "denominazione", "com_name"}
	codeKeys = []string{"com_istat_code", "pro_com_t", "pro_com", "cod_istat", "istat", "code"}
)

// pick returns the first non-empty property among keys, ignoring case.
func pick(properties map[string]string, keys []string) string {
	for _, key := range keys {
		for name, value := range properties {
			if strings.EqualFold(name, key) && strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return ""
}
