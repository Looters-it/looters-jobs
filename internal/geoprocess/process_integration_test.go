package geoprocess_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/config"
	"gioco/jobs/internal/geoprocess"
)

const milanFile = "../geo/testdata/milano-comuni.geojson"

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
type fixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	processor *geoprocess.Processor
	seq       int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Removes units, uploads and adjacency with them.
	if _, err := pool.Exec(context.Background(), `DELETE FROM "GeoArea"`); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, pool: pool, processor: &geoprocess.Processor{
		Pool:      pool,
		UploadDir: t.TempDir(),
		Geo:       config.GeoConfig{SimplifyToleranceM: 50, AdjacencyGapM: 5, AdjacencyMinBorderM: 50},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) area(name string, parent *string) string {
	f.seq++
	id := fmt.Sprintf("area-%d", f.seq)
	f.exec(`INSERT INTO "GeoArea" (id, name, "parentId") VALUES ($1, $2, $3)`, id, name, parent)
	return id
}

// upload stores content as an uploaded file, processes it and returns its id.
func (f *fixture) upload(areaID, format string, content []byte) string {
	f.t.Helper()
	f.seq++
	id := fmt.Sprintf("upload-%d", f.seq)
	if err := os.WriteFile(filepath.Join(f.processor.UploadDir, id), content, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO "GeoUpload" (id, "areaId", filename, format, "storagePath") VALUES ($1, $2, 'file', $3::"GeoFormat", $1)`,
		id, areaID, format)
	if err := f.processor.Process(context.Background(), id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

type uploadState struct {
	Status      string
	Progress    int
	Units       int
	Adjacencies int
	Failure     geoprocess.Failure
}

func (f *fixture) state(uploadID string) uploadState {
	f.t.Helper()
	var state uploadState
	var failure *string
	err := f.pool.QueryRow(context.Background(),
		`SELECT status::text, progress, "unitCount", "adjacencyCount", error FROM "GeoUpload" WHERE id = $1`, uploadID,
	).Scan(&state.Status, &state.Progress, &state.Units, &state.Adjacencies, &failure)
	if err != nil {
		f.t.Fatal(err)
	}
	if failure != nil {
		if err := json.Unmarshal([]byte(*failure), &state.Failure); err != nil {
			f.t.Fatalf("error is not JSON: %s", *failure)
		}
	}
	return state
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) neighbours(name string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT o.name FROM "GeoUnit" u
		JOIN "GeoAdjacency" adj ON u.id IN (adj."aId", adj."bId")
		JOIN "GeoUnit" o ON o.id IN (adj."aId", adj."bId") AND o.id <> u.id
		WHERE u.name = $1 ORDER BY o.name`, name)
	if err != nil {
		f.t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return names
}

func milan(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(milanFile)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestRealBoundariesOfMilanProvince(t *testing.T) {
	f := setup(t)
	id := f.upload(f.area("Italia", nil), "GEOJSON", milan(t))

	state := f.state(id)
	if state.Status != "DONE" || state.Progress != 100 || state.Units != 133 {
		t.Fatalf("upload state: %+v", state)
	}
	if got := f.count(`SELECT count(*) FROM "GeoUnit" WHERE "uploadId" = $1`, id); got != 133 {
		t.Fatalf("stored %d municipalities, want 133", got)
	}
	if got := f.count(`SELECT count(*) FROM "GeoAdjacency"`); got != state.Adjacencies || got < 300 {
		t.Fatalf("%d adjacencies stored, upload reports %d", got, state.Adjacencies)
	}

	// Adjacency of known cases, checked by hand on the map.
	milano := f.neighbours("Milano")
	for _, neighbour := range []string{"Sesto San Giovanni", "Segrate", "Rozzano", "Corsico", "Pero"} {
		if !slices.Contains(milano, neighbour) {
			t.Errorf("Milano should border %s; neighbours: %v", neighbour, milano)
		}
	}
	for _, far := range []string{"Abbiategrasso", "Legnano", "Magenta"} {
		if slices.Contains(milano, far) {
			t.Errorf("Milano should not border %s", far)
		}
	}
	abbiategrasso := f.neighbours("Abbiategrasso")
	for _, neighbour := range []string{"Ozzero", "Morimondo"} {
		if !slices.Contains(abbiategrasso, neighbour) {
			t.Errorf("Abbiategrasso should border %s; neighbours: %v", neighbour, abbiategrasso)
		}
	}
	// An exclave of the province: none of its neighbours is in this file.
	if got := f.neighbours("San Colombano al Lambro"); len(got) != 0 {
		t.Errorf("San Colombano al Lambro should have no neighbours here, got %v", got)
	}
	// Stored once per pair, smaller id first, never manual.
	if got := f.count(`SELECT count(*) FROM "GeoAdjacency" WHERE "aId" >= "bId" OR manual`); got != 0 {
		t.Errorf("%d malformed adjacency rows", got)
	}

	// Geometry: valid, located where Milan is, and lighter for display.
	if got := f.count(`SELECT count(*) FROM "GeoUnit" WHERE NOT ST_IsValid(geom) OR ST_IsEmpty("geomSimple") OR ST_SRID(geom) <> 4326`); got != 0 {
		t.Errorf("%d municipalities with a bad geometry", got)
	}
	if got := f.count(`SELECT count(*) FROM "GeoUnit" WHERE name = 'Milano'
		AND "centroidLon" BETWEEN 9.0 AND 9.3 AND "centroidLat" BETWEEN 45.35 AND 45.6 AND "areaKm2" BETWEEN 170 AND 195`); got != 1 {
		t.Error("Milano has an unexpected centroid or area")
	}
	full := f.count(`SELECT sum(ST_NPoints(geom))::int FROM "GeoUnit"`)
	simple := f.count(`SELECT sum(ST_NPoints("geomSimple"))::int FROM "GeoUnit"`)
	if simple >= full {
		t.Errorf("display geometry is not simpler: %d points against %d", simple, full)
	}
	// Shared borders stay shared after simplification: no overlaps between neighbours.
	if got := f.count(`SELECT count(*) FROM "GeoUnit" a JOIN "GeoUnit" b ON a.id < b.id AND ST_Overlaps(a."geomSimple", b."geomSimple")
		WHERE ST_Area(ST_Intersection(a."geomSimple", b."geomSimple")::geography) > 100`); got != 0 {
		t.Errorf("%d pairs of simplified municipalities overlap", got)
	}

	// Processing a finished upload again changes nothing.
	if err := f.processor.Process(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := f.count(`SELECT count(*) FROM "GeoUnit"`); got != 133 {
		t.Errorf("reprocessing changed the municipalities: %d", got)
	}
}

func TestRegionsUploadedSeparatelyJoinUp(t *testing.T) {
	f := setup(t)
	var collection struct {
		Type     string            `json:"type"`
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(milan(t), &collection); err != nil {
		t.Fatal(err)
	}
	// The file is sorted by name: Corsico falls in the first half, Milano in the second.
	half := len(collection.Features) / 2
	part := func(features []json.RawMessage) []byte {
		body, _ := json.Marshal(map[string]any{"type": "FeatureCollection", "features": features})
		return body
	}

	italy := f.area("Italia", nil)
	west := f.upload(f.area("Ovest", &italy), "GEOJSON", part(collection.Features[:half]))
	east := f.upload(f.area("Est", &italy), "GEOJSON", part(collection.Features[half:]))
	if f.state(west).Status != "DONE" || f.state(east).Status != "DONE" {
		t.Fatalf("uploads: %+v %+v", f.state(west), f.state(east))
	}
	if !slices.Contains(f.neighbours("Milano"), "Corsico") {
		t.Errorf("Milano and Corsico were uploaded separately and should still be adjacent: %v", f.neighbours("Milano"))
	}

	// The same municipalities again, anywhere in the same area, are refused.
	again := f.upload(italy, "GEOJSON", part(collection.Features[:3]))
	state := f.state(again)
	if state.Status != "FAILED" || state.Failure.Code != geoprocess.FailureAlreadyLoaded || len(state.Failure.Names) != 3 {
		t.Fatalf("duplicate upload: %+v", state)
	}
	// In another geographic area they are independent, and not adjacent to the first ones.
	other := f.upload(f.area("Prova", nil), "GEOJSON", part(collection.Features[:3]))
	if f.state(other).Status != "DONE" {
		t.Fatalf("upload in another area: %+v", f.state(other))
	}
	if got := f.count(`SELECT count(*) FROM "GeoAdjacency" adj JOIN "GeoUnit" a ON a.id = adj."aId" JOIN "GeoUnit" b ON b.id = adj."bId"
		WHERE (a."uploadId" = $1) <> (b."uploadId" = $1)`, other); got != 0 {
		t.Errorf("%d adjacencies cross two geographic areas", got)
	}
}

func TestRefusedFilesSaveNothingAndExplainWhy(t *testing.T) {
	f := setup(t)
	area := f.area("Italia", nil)
	square := `{"type":"Polygon","coordinates":[[[9,45],[9.1,45],[9.1,45.1],[9,45.1],[9,45]]]}`
	cases := map[string]struct {
		format, content, code string
	}{
		"not json":       {"GEOJSON", `{"type":`, "INVALID_FILE"},
		"not collection": {"GEOJSON", `{"type":"Feature"}`, "NOT_FEATURE_COLLECTION"},
		"empty":          {"GEOJSON", `{"type":"FeatureCollection","features":[]}`, geoprocess.FailureNoUnits},
		"bad kml":        {"KML", `<kml><Placemark>`, "INVALID_FILE"},
		"one bad feature": {"GEOJSON", `{"type":"FeatureCollection","features":[
			{"type":"Feature","properties":{"name":"Valido"},"geometry":` + square + `},
			{"type":"Feature","properties":{},"geometry":` + square + `}]}`, geoprocess.FailureFeatureProblems},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := f.state(f.upload(area, tc.format, []byte(tc.content)))
			if state.Status != "FAILED" || state.Failure.Code != tc.code {
				t.Fatalf("state: %+v", state)
			}
		})
	}
	state := f.state(f.upload(area, "GEOJSON", []byte(cases["one bad feature"].content)))
	if state.Failure.Total != 1 || len(state.Failure.Problems) != 1 || state.Failure.Problems[0].Feature != 2 {
		t.Errorf("problems: %+v", state.Failure)
	}
	// One bad feature refuses the whole file: nothing is half-loaded.
	if got := f.count(`SELECT count(*) FROM "GeoUnit"`); got != 0 {
		t.Errorf("%d municipalities saved from refused files", got)
	}
}

func TestKMLUpload(t *testing.T) {
	f := setup(t)
	ring := func(x float64) string {
		return fmt.Sprintf("%[1]g,45 %[2]g,45 %[2]g,45.1 %[1]g,45.1 %[1]g,45", x, x+0.1)
	}
	placemark := func(name string, x float64) string {
		return `<Placemark><name>` + name + `</name><Polygon><outerBoundaryIs><LinearRing><coordinates>` +
			ring(x) + `</coordinates></LinearRing></outerBoundaryIs></Polygon></Placemark>`
	}
	// Three squares in a row: A touches B, B touches C, A and C do not touch.
	kml := `<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>` +
		placemark("A", 9) + placemark("B", 9.1) + placemark("C", 9.2) + `</Document></kml>`

	state := f.state(f.upload(f.area("Prova", nil), "KML", []byte(kml)))
	if state.Status != "DONE" || state.Units != 3 || state.Adjacencies != 2 {
		t.Fatalf("state: %+v", state)
	}
	if got := f.neighbours("B"); !slices.Equal(got, []string{"A", "C"}) {
		t.Errorf("neighbours of B: %v", got)
	}
}

func TestCornerContactIsNotAdjacency(t *testing.T) {
	f := setup(t)
	square := func(name string, x, y float64) string {
		return fmt.Sprintf(`{"type":"Feature","properties":{"name":%q},"geometry":{"type":"Polygon","coordinates":[[[%[2]g,%[3]g],[%[4]g,%[3]g],[%[4]g,%[5]g],[%[2]g,%[5]g],[%[2]g,%[3]g]]]}}`,
			name, x, y, x+0.1, y+0.1)
	}
	// SW and NE meet only at the corner (9.1, 45.1); SW and SE share a side.
	content := `{"type":"FeatureCollection","features":[` +
		square("SW", 9, 45) + `,` + square("SE", 9.1, 45) + `,` + square("NE", 9.1, 45.1) + `]}`
	f.upload(f.area("Prova", nil), "GEOJSON", []byte(content))

	if got := f.neighbours("SW"); !slices.Equal(got, []string{"SE"}) {
		t.Errorf("neighbours of SW: %v (the corner contact with NE must not count)", got)
	}
	if got := f.neighbours("NE"); !slices.Equal(got, []string{"SE"}) {
		t.Errorf("neighbours of NE: %v", got)
	}
}

// The whole of Italy (about 7,900 municipalities). Set GEO_ITALY_FILE to a
// GeoJSON with every Italian municipality to run it; it takes a few minutes.
// The dataset used to write these checks is openpolis/geojson-italy.
func TestRealBoundariesOfItaly(t *testing.T) {
	path := os.Getenv("GEO_ITALY_FILE")
	if path == "" {
		t.Skip("GEO_ITALY_FILE not set")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := setup(t)
	id := f.upload(f.area("Italia", nil), "GEOJSON", content)

	state := f.state(id)
	if state.Status != "DONE" {
		t.Fatalf("upload state: %+v", state)
	}
	t.Logf("%d municipalities, %d adjacencies", state.Units, state.Adjacencies)
	if state.Units < 7800 || state.Units > 8200 {
		t.Errorf("%d municipalities: not the whole of Italy", state.Units)
	}
	if average := float64(2*state.Adjacencies) / float64(state.Units); average < 4.5 || average > 7 {
		t.Errorf("%.1f neighbours per municipality on average, expected between 4.5 and 7", average)
	}

	borders := map[string][]string{
		"Roma":   {"Fiumicino", "Pomezia", "Tivoli"},
		"Torino": {"Moncalieri", "Venaria Reale"},
		"Napoli": {"Pozzuoli", "Portici"},
		"Capri":  {"Anacapri"},
		// Across a regional border, along the Ticino river.
		"Sesto Calende": {"Castelletto sopra Ticino"},
	}
	for name, expected := range borders {
		got := f.neighbours(name)
		for _, neighbour := range expected {
			if !slices.Contains(got, neighbour) {
				t.Errorf("%s should border %s; neighbours: %v", name, neighbour, got)
			}
		}
	}
	if got := f.neighbours("Capri"); len(got) != 1 {
		t.Errorf("Capri should border Anacapri only, got %v", got)
	}
	// No land border: these need the admin's manual adjacencies.
	for _, isolated := range []string{"Campione d'Italia", "Lampedusa e Linosa", "Pantelleria", "Ustica"} {
		if got := f.neighbours(isolated); len(got) != 0 {
			t.Errorf("%s should have no neighbours, got %v", isolated, got)
		}
	}
	// The strait is not a border.
	if slices.Contains(f.neighbours("Messina"), "Villa San Giovanni") {
		t.Error("Messina and Villa San Giovanni are separated by the sea")
	}
}
