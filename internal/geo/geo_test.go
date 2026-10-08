package geo

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const square = `[[[9.0,45.0],[9.1,45.0],[9.1,45.1],[9.0,45.1],[9.0,45.0]]]`

func feature(properties, geometry string) string {
	return `{"type":"Feature","properties":` + properties + `,"geometry":` + geometry + `}`
}

func collection(features ...string) string {
	return `{"type":"FeatureCollection","bbox":[0,0,1,1],"features":[` + strings.Join(features, ",") + `]}`
}

func parse(t *testing.T, format, content string) ([]Unit, []Problem) {
	t.Helper()
	units, problems, err := Parse(format, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return units, problems
}

func multiPolygon(t *testing.T, unit Unit) [][][][2]float64 {
	t.Helper()
	var geometry struct {
		Type        string
		Coordinates [][][][2]float64
	}
	if err := json.Unmarshal(unit.Geometry, &geometry); err != nil || geometry.Type != "MultiPolygon" {
		t.Fatalf("not a MultiPolygon: %s (%v)", unit.Geometry, err)
	}
	return geometry.Coordinates
}

func TestGeoJSONPolygonAndMultiPolygon(t *testing.T) {
	units, problems := parse(t, FormatGeoJSON, collection(
		feature(`{"name":" Milano ","com_istat_code":"015146"}`, `{"type":"Polygon","coordinates":`+square+`}`),
		feature(`{"COMUNE":"Monza","PRO_COM":108033}`, `{"type":"MultiPolygon","coordinates":[`+square+`,`+square+`]}`),
		// Altitude is dropped.
		feature(`{"nome":"Lodi"}`, `{"type":"Polygon","coordinates":[[[9,45,120],[9.1,45,120],[9.1,45.1,120],[9,45,120]]]}`),
	))
	if len(problems) != 0 || len(units) != 3 {
		t.Fatalf("units=%d problems=%+v", len(units), problems)
	}
	if units[0].Name != "Milano" || units[0].Code != "015146" || len(multiPolygon(t, units[0])) != 1 {
		t.Errorf("first unit: %+v", units[0])
	}
	if units[1].Name != "Monza" || units[1].Code != "108033" || len(multiPolygon(t, units[1])) != 2 {
		t.Errorf("second unit: %+v", units[1])
	}
	if units[2].Code != "" || multiPolygon(t, units[2])[0][0][0] != [2]float64{9, 45} {
		t.Errorf("third unit: %+v %s", units[2], units[2].Geometry)
	}
}

func TestGeoJSONReportsEachProblemWithItsPosition(t *testing.T) {
	polygon := `{"type":"Polygon","coordinates":` + square + `}`
	units, problems := parse(t, FormatGeoJSON, collection(
		feature(`{"name":"Valido","code":"1"}`, polygon),
		feature(`{}`, polygon),
		feature(`{"name":"Punto"}`, `{"type":"Point","coordinates":[9,45]}`),
		feature(`{"name":"Nullo"}`, `null`),
		// Projected coordinates, in metres.
		feature(`{"name":"Metri"}`, `{"type":"Polygon","coordinates":[[[514000,5034000],[515000,5034000],[515000,5035000],[514000,5034000]]]}`),
		feature(`{"name":"Corto"}`, `{"type":"Polygon","coordinates":[[[9,45],[9.1,45],[9,45]]]}`),
		feature(`{"name":"Doppione","code":"1"}`, polygon),
	))
	if len(units) != 1 || units[0].Name != "Valido" {
		t.Fatalf("units: %+v", units)
	}
	want := []Problem{
		{2, "", ProblemMissingName},
		{3, "Punto", ProblemNoPolygon},
		{4, "Nullo", ProblemNoPolygon},
		{5, "Metri", ProblemBadCoordinates},
		{6, "Corto", ProblemRingTooShort},
		{7, "Doppione", ProblemDuplicateCode},
	}
	if len(problems) != len(want) {
		t.Fatalf("problems: %+v", problems)
	}
	for i := range want {
		if problems[i] != want[i] {
			t.Errorf("problem %d = %+v, want %+v", i, problems[i], want[i])
		}
	}
}

func TestGeoJSONFileErrors(t *testing.T) {
	cases := map[string]string{
		`not json`: "INVALID_FILE",
		`[1,2]`:    "INVALID_FILE",
		`{"type":"FeatureCollection","features":[{"type":`: "INVALID_FILE",
		`{"type":"Feature","properties":{}}`:               "NOT_FEATURE_COLLECTION",
		`{"type":"Topology","features":[]}`:                "NOT_FEATURE_COLLECTION",
		`{"type":"FeatureCollection","features":{}}`:       "NOT_FEATURE_COLLECTION",
	}
	for content, code := range cases {
		_, _, err := Parse(FormatGeoJSON, strings.NewReader(content))
		var fileErr *FileError
		if !errors.As(err, &fileErr) || fileErr.Code != code {
			t.Errorf("%s: got %v, want %s", content, err, code)
		}
	}
}

const kmlDocument = `<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2"><Document><name>Comuni</name>
<Folder>
  <Placemark><name>Milano</name>
    <ExtendedData><SchemaData schemaUrl="#s"><SimpleData name="PRO_COM_T">015146</SimpleData></SchemaData></ExtendedData>
    <Polygon>
      <outerBoundaryIs><LinearRing><coordinates>
        9.0,45.0,0 9.2,45.0,0 9.2,45.2,0 9.0,45.2,0 9.0,45.0,0
      </coordinates></LinearRing></outerBoundaryIs>
      <innerBoundaryIs><LinearRing><coordinates>9.05,45.05 9.1,45.05 9.1,45.1 9.05,45.05</coordinates></LinearRing></innerBoundaryIs>
    </Polygon>
  </Placemark>
  <Placemark>
    <ExtendedData><Data name="comune"><value>Isole</value></Data><Data name="istat"><value>999</value></Data></ExtendedData>
    <MultiGeometry>
      <Polygon><outerBoundaryIs><LinearRing><coordinates>8,44 8.1,44 8.1,44.1 8,44</coordinates></LinearRing></outerBoundaryIs></Polygon>
      <MultiGeometry><Polygon><outerBoundaryIs><LinearRing><coordinates>7,44 7.1,44 7.1,44.1 7,44</coordinates></LinearRing></outerBoundaryIs></Polygon></MultiGeometry>
    </MultiGeometry>
  </Placemark>
</Folder>
<Placemark><name>Punto</name><Point><coordinates>9,45</coordinates></Point></Placemark>
<Placemark><name>Rotto</name><Polygon><outerBoundaryIs><LinearRing><coordinates>9,45 x,y 9,45 9,45</coordinates></LinearRing></outerBoundaryIs></Polygon></Placemark>
</Document></kml>`

func TestKML(t *testing.T) {
	units, problems := parse(t, FormatKML, kmlDocument)
	if len(units) != 2 {
		t.Fatalf("units: %+v", units)
	}
	milano := multiPolygon(t, units[0])
	if units[0].Name != "Milano" || units[0].Code != "015146" || len(milano) != 1 || len(milano[0]) != 2 || len(milano[0][0]) != 5 {
		t.Errorf("Milano: %+v %s", units[0], units[0].Geometry)
	}
	if units[1].Name != "Isole" || units[1].Code != "999" || len(multiPolygon(t, units[1])) != 2 {
		t.Errorf("Isole: %+v %s", units[1], units[1].Geometry)
	}
	want := []Problem{{3, "Punto", ProblemNoPolygon}, {4, "Rotto", ProblemBadCoordinates}}
	if len(problems) != 2 || problems[0] != want[0] || problems[1] != want[1] {
		t.Errorf("problems: %+v", problems)
	}
}

func TestKMLFileErrors(t *testing.T) {
	for _, content := range []string{``, `<html></html>`, `<kml><Placemark><name>x</name>`} {
		_, _, err := Parse(FormatKML, strings.NewReader(content))
		var fileErr *FileError
		if !errors.As(err, &fileErr) || fileErr.Code != "INVALID_FILE" {
			t.Errorf("%q: got %v", content, err)
		}
	}
}

func TestRealBoundariesOfMilanProvince(t *testing.T) {
	file, err := os.Open("testdata/milano-comuni.geojson")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	units, problems, err := Parse(FormatGeoJSON, file)
	if err != nil || len(problems) != 0 {
		t.Fatalf("err=%v problems=%+v", err, problems)
	}
	if len(units) != 133 {
		t.Fatalf("got %d municipalities, want 133", len(units))
	}
	if units[0].Name != "Abbiategrasso" || units[0].Code != "015002" {
		t.Errorf("first unit: %s %s", units[0].Name, units[0].Code)
	}
}
