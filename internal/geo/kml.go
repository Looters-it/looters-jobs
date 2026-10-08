package geo

import (
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
)

type kmlPolygon struct {
	Outer string   `xml:"outerBoundaryIs>LinearRing>coordinates"`
	Inner []string `xml:"innerBoundaryIs>LinearRing>coordinates"`
}

type kmlMultiGeometry struct {
	Polygons []kmlPolygon       `xml:"Polygon"`
	Nested   []kmlMultiGeometry `xml:"MultiGeometry"`
}

type kmlPlacemark struct {
	Name     string `xml:"name"`
	Extended struct {
		Data []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value"`
		} `xml:"Data"`
		Simple []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"SchemaData>SimpleData"`
	} `xml:"ExtendedData"`
	kmlMultiGeometry // a Placemark holds a Polygon or a MultiGeometry directly
}

// parseKML streams the Placemarks of a KML document, wherever they are
// nested (Document, Folder).
func parseKML(r io.Reader, c *collector) error {
	dec := xml.NewDecoder(r)
	sawRoot := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return &FileError{Code: "INVALID_FILE", Detail: err.Error()}
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !sawRoot {
			sawRoot = true
			if start.Name.Local != "kml" {
				return &FileError{Code: "INVALID_FILE", Detail: "the root element is not <kml>"}
			}
			continue
		}
		if start.Name.Local != "Placemark" {
			continue
		}
		var placemark kmlPlacemark
		if err := dec.DecodeElement(&placemark, &start); err != nil {
			return &FileError{Code: "INVALID_FILE", Detail: err.Error()}
		}
		addKMLPlacemark(c, placemark)
	}
	if !sawRoot {
		return &FileError{Code: "INVALID_FILE", Detail: "the file is empty"}
	}
	return nil
}

func addKMLPlacemark(c *collector, placemark kmlPlacemark) {
	properties := map[string]string{}
	for _, data := range placemark.Extended.Data {
		properties[data.Name] = data.Value
	}
	for _, data := range placemark.Extended.Simple {
		properties[data.Name] = data.Value
	}
	name := placemark.Name
	if strings.TrimSpace(name) == "" {
		name = pick(properties, nameKeys)
	}

	polygons, ok := kmlPolygons(placemark.kmlMultiGeometry)
	if !ok {
		c.reject(name, ProblemBadCoordinates)
		return
	}
	c.add(name, pick(properties, codeKeys), polygons)
}

// kmlPolygons flattens nested MultiGeometry elements. It reports false when
// a coordinate list cannot be read.
func kmlPolygons(geometry kmlMultiGeometry) ([]polygon, bool) {
	var polygons []polygon
	for _, p := range geometry.Polygons {
		poly := polygon{}
		for _, text := range append([]string{p.Outer}, p.Inner...) {
			r, ok := kmlRing(text)
			if !ok {
				return nil, false
			}
			poly = append(poly, r)
		}
		polygons = append(polygons, poly)
	}
	for _, nested := range geometry.Nested {
		more, ok := kmlPolygons(nested)
		if !ok {
			return nil, false
		}
		polygons = append(polygons, more...)
	}
	return polygons, true
}

// kmlRing reads "lon,lat[,alt] lon,lat[,alt] ...".
func kmlRing(text string) (ring, bool) {
	fields := strings.Fields(text)
	r := make(ring, 0, len(fields))
	for _, field := range fields {
		parts := strings.Split(field, ",")
		if len(parts) < 2 {
			return nil, false
		}
		lon, errLon := strconv.ParseFloat(parts[0], 64)
		lat, errLat := strconv.ParseFloat(parts[1], 64)
		if errLon != nil || errLat != nil {
			return nil, false
		}
		r = append(r, [2]float64{lon, lat})
	}
	return r, true
}
