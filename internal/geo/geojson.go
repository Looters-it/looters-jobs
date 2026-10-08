package geo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

type geoJSONFeature struct {
	Properties map[string]any `json:"properties"`
	Geometry   *struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
}

// parseGeoJSON streams a FeatureCollection, one feature at a time, so that a
// file with thousands of municipalities is never held in memory as a whole.
func parseGeoJSON(r io.Reader, c *collector) error {
	dec := json.NewDecoder(r)
	invalid := func(err error) error { return &FileError{Code: "INVALID_FILE", Detail: err.Error()} }

	if tok, err := dec.Token(); err != nil {
		return invalid(err)
	} else if tok != json.Delim('{') {
		return invalid(errors.New("the file is not a JSON object"))
	}

	foundFeatures := false
	rootType := ""
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return invalid(err)
		}
		switch keyTok {
		case "type":
			if err := dec.Decode(&rootType); err != nil {
				return invalid(err)
			}
		case "features":
			foundFeatures = true
			if tok, err := dec.Token(); err != nil {
				return invalid(err)
			} else if tok != json.Delim('[') {
				return &FileError{Code: "NOT_FEATURE_COLLECTION", Detail: `"features" is not an array`}
			}
			for dec.More() {
				var feature geoJSONFeature
				if err := dec.Decode(&feature); err != nil {
					return invalid(err)
				}
				addGeoJSONFeature(c, feature)
			}
			if _, err := dec.Token(); err != nil { // closing ]
				return invalid(err)
			}
		default:
			var skipped json.RawMessage
			if err := dec.Decode(&skipped); err != nil {
				return invalid(err)
			}
		}
	}
	if _, err := dec.Token(); err != nil { // closing }
		return invalid(err)
	}
	if !foundFeatures || (rootType != "" && rootType != "FeatureCollection") {
		return &FileError{Code: "NOT_FEATURE_COLLECTION", Detail: fmt.Sprintf("root type is %q", rootType)}
	}
	return nil
}

func addGeoJSONFeature(c *collector, feature geoJSONFeature) {
	properties := make(map[string]string, len(feature.Properties))
	for key, value := range feature.Properties {
		switch v := value.(type) {
		case string:
			properties[key] = v
		case float64:
			properties[key] = strconv.FormatFloat(v, 'f', -1, 64)
		}
	}

	var polygons []polygon
	if g := feature.Geometry; g != nil {
		switch g.Type {
		case "Polygon":
			var one [][][]float64
			if json.Unmarshal(g.Coordinates, &one) == nil {
				polygons = toPolygons([][][][]float64{one})
			}
		case "MultiPolygon":
			var many [][][][]float64
			if json.Unmarshal(g.Coordinates, &many) == nil {
				polygons = toPolygons(many)
			}
		}
	}
	c.add(pick(properties, nameKeys), pick(properties, codeKeys), polygons)
}

// toPolygons keeps longitude and latitude of each position, dropping any
// altitude. A position with fewer than two numbers makes the result nil.
func toPolygons(raw [][][][]float64) []polygon {
	polygons := make([]polygon, 0, len(raw))
	for _, rawPolygon := range raw {
		poly := make(polygon, 0, len(rawPolygon))
		for _, rawRing := range rawPolygon {
			r := make(ring, 0, len(rawRing))
			for _, position := range rawRing {
				if len(position) < 2 {
					return nil
				}
				r = append(r, [2]float64{position[0], position[1]})
			}
			poly = append(poly, r)
		}
		polygons = append(polygons, poly)
	}
	return polygons
}
