// Package geoprocess turns an uploaded file into municipalities, their
// display geometry and their adjacency. Parsing happens in Go (package geo);
// every geometric computation is delegated to PostGIS.
package geoprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/config"
	"gioco/jobs/internal/geo"
)

// metresPerDegree converts the configured distances to degrees. It is the
// length of a degree of latitude; at Italian latitudes a degree of longitude
// is shorter, so east-west distances come out about 30% tighter.
const metresPerDegree = 111_320.0

// maxReported caps the per-feature problems stored for the admin.
const maxReported = 50

// Failure is what the admin sees when an upload is refused. It is stored as
// JSON in GeoUpload.error; the web interface translates the codes.
type Failure struct {
	Code string `json:"code"`
	// Detail is free text from the parser or the database, in English.
	Detail string `json:"detail,omitempty"`
	// Problems lists the first features at fault; Total counts all of them.
	Problems []geo.Problem `json:"problems,omitempty"`
	Total    int           `json:"total,omitempty"`
	// Names lists municipalities, for ALREADY_LOADED and EMPTY_GEOMETRY.
	Names []string `json:"names,omitempty"`
}

// Failure codes besides the ones of geo.FileError.
const (
	FailureFileMissing     = "FILE_MISSING"
	FailureNoUnits         = "NO_UNITS"
	FailureFeatureProblems = "FEATURE_PROBLEMS"
	FailureAlreadyLoaded   = "ALREADY_LOADED"
	FailureEmptyGeometry   = "EMPTY_GEOMETRY"
	FailureInternal        = "INTERNAL"
)

// Processor handles `geo.process` jobs.
type Processor struct {
	Pool      *pgxpool.Pool
	UploadDir string
	Geo       config.GeoConfig
	Log       *slog.Logger
}

type upload struct {
	id, areaID, format, storagePath, status string
}

// Process runs the whole pipeline for one upload. A refused file is not an
// error of the job: the reason is recorded on the upload and nil is returned.
// Running it again for a finished upload does nothing.
func (p *Processor) Process(ctx context.Context, uploadID string) error {
	u := upload{id: uploadID}
	err := p.Pool.QueryRow(ctx,
		`SELECT "areaId", format::text, "storagePath", status::text FROM "GeoUpload" WHERE id = $1`, uploadID,
	).Scan(&u.areaID, &u.format, &u.storagePath, &u.status)
	if errors.Is(err, pgx.ErrNoRows) {
		p.Log.Warn("upload deleted before processing", "uploadId", uploadID)
		return nil
	}
	if err != nil {
		return err
	}
	if u.status == "DONE" || u.status == "FAILED" {
		return nil
	}
	if _, err := p.Pool.Exec(ctx,
		`UPDATE "GeoUpload" SET status = 'PROCESSING', progress = 0, error = NULL WHERE id = $1`, uploadID); err != nil {
		return err
	}

	failure, err := p.run(ctx, u)
	if err != nil {
		// Unexpected: record it so the admin is not left with a stuck upload.
		p.Log.Error("geo processing failed", "uploadId", uploadID, "err", err)
		failure = &Failure{Code: FailureInternal, Detail: err.Error()}
	}
	if failure != nil {
		return p.fail(ctx, uploadID, *failure)
	}
	return nil
}

func (p *Processor) fail(ctx context.Context, uploadID string, failure Failure) error {
	body, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	// The context may be the reason of the failure (shutdown): record it anyway.
	_, err = p.Pool.Exec(context.WithoutCancel(ctx),
		`UPDATE "GeoUpload" SET status = 'FAILED', error = $2, "finishedAt" = (now() AT TIME ZONE 'UTC') WHERE id = $1`,
		uploadID, string(body))
	return err
}

func (p *Processor) progress(ctx context.Context, uploadID string, percent int) {
	// Outside the transaction, so the admin sees it while the work goes on.
	if _, err := p.Pool.Exec(ctx, `UPDATE "GeoUpload" SET progress = $2 WHERE id = $1`, uploadID, percent); err != nil {
		p.Log.Warn("progress update failed", "uploadId", uploadID, "err", err)
	}
}

// run returns a Failure when the file is refused and an error when something
// unexpected breaks. Nothing is saved in either case.
func (p *Processor) run(ctx context.Context, u upload) (*Failure, error) {
	// 1. Validate and convert to the internal form.
	file, err := os.Open(filepath.Join(p.UploadDir, filepath.Base(u.storagePath)))
	if err != nil {
		return &Failure{Code: FailureFileMissing, Detail: err.Error()}, nil
	}
	units, problems, err := geo.Parse(u.format, file)
	file.Close()
	var fileErr *geo.FileError
	if errors.As(err, &fileErr) {
		return &Failure{Code: fileErr.Code, Detail: fileErr.Detail}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return &Failure{Code: FailureFeatureProblems, Total: len(problems), Problems: problems[:min(len(problems), maxReported)]}, nil
	}
	if len(units) == 0 {
		return &Failure{Code: FailureNoUnits}, nil
	}
	p.progress(ctx, u.id, 15)

	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Adjacency and duplicates are checked inside the whole geographic area
	// (the root folder), so that regions uploaded one by one join up.
	var tree []string
	err = tx.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, "parentId" FROM "GeoArea" WHERE id = $1
			UNION ALL
			SELECT a.id, a."parentId" FROM "GeoArea" a JOIN up ON a.id = up."parentId"
		), down AS (
			SELECT id FROM up WHERE "parentId" IS NULL
			UNION ALL
			SELECT a.id FROM "GeoArea" a JOIN down ON a."parentId" = down.id
		)
		SELECT array_agg(id) FROM down`, u.areaID).Scan(&tree)
	if err != nil {
		return nil, err
	}

	// A retry starts from scratch.
	if _, err := tx.Exec(ctx, `DELETE FROM "GeoUnit" WHERE "uploadId" = $1`, u.id); err != nil {
		return nil, err
	}

	codes := make([]string, 0, len(units))
	for _, unit := range units {
		if unit.Code != "" {
			codes = append(codes, unit.Code)
		}
	}
	if names, err := names(ctx, tx,
		`SELECT name FROM "GeoUnit" WHERE "areaId" = ANY($1) AND code = ANY($2) ORDER BY name LIMIT 10`, tree, codes); err != nil {
		return nil, err
	} else if len(names) > 0 {
		return &Failure{Code: FailureAlreadyLoaded, Names: names}, nil
	}

	// 2. Save each municipality with its geometry.
	const insert = `
		INSERT INTO "GeoUnit" ("areaId", "uploadId", name, code, "centroidLon", "centroidLat", "areaKm2", geom, "geomSimple")
		SELECT $1, $2, $3, NULLIF($4, ''), ST_X(point), ST_Y(point), ST_Area(g::geography) / 1e6, g, g
		FROM (
			SELECT g, CASE WHEN ST_IsEmpty(g) THEN ST_SetSRID(ST_MakePoint(0, 0), 4326) ELSE ST_PointOnSurface(g) END AS point
			FROM (SELECT ST_Multi(ST_CollectionExtract(ST_MakeValid(ST_SetSRID(ST_GeomFromGeoJSON($5), 4326)), 3)) AS g) made
		) placed`
	const batchSize = 200
	for start := 0; start < len(units); start += batchSize {
		batch := &pgx.Batch{}
		for _, unit := range units[start:min(start+batchSize, len(units))] {
			batch.Queue(insert, u.areaID, u.id, unit.Name, unit.Code, string(unit.Geometry))
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return nil, fmt.Errorf("save municipalities: %w", err)
		}
		p.progress(ctx, u.id, 15+40*min(start+batchSize, len(units))/len(units))
	}
	if names, err := names(ctx, tx,
		`SELECT name FROM "GeoUnit" WHERE "uploadId" = $1 AND ST_IsEmpty(geom) ORDER BY name LIMIT 10`, u.id); err != nil {
		return nil, err
	} else if len(names) > 0 {
		return &Failure{Code: FailureEmptyGeometry, Names: names}, nil
	}

	// 3. Simplify for display.
	if err := p.simplify(ctx, tx, u.id); err != nil {
		return nil, fmt.Errorf("simplify: %w", err)
	}
	p.progress(ctx, u.id, 70)

	// 4. Adjacency, computed once and stored.
	adjacencies, err := p.adjacency(ctx, tx, u.id, tree)
	if err != nil {
		return nil, fmt.Errorf("adjacency: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE "GeoUpload" SET status = 'DONE', progress = 100, "unitCount" = $2, "adjacencyCount" = $3,
			"finishedAt" = (now() AT TIME ZONE 'UTC')
		WHERE id = $1`, u.id, len(units), adjacencies); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	p.Log.Info("geo upload processed", "uploadId", u.id, "units", len(units), "adjacencies", adjacencies)
	return nil, nil
}

// simplify fills geomSimple. Municipalities form a coverage (they share
// borders without overlapping), so the coverage algorithm is used: a shared
// border is simplified once and stays shared, leaving no gaps on the map.
// If the data is not a clean coverage it falls back to simplifying each
// polygon on its own.
func (p *Processor) simplify(ctx context.Context, tx pgx.Tx, uploadID string) error {
	tolerance := p.Geo.SimplifyToleranceM / metresPerDegree

	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = savepoint.Exec(ctx, `
		UPDATE "GeoUnit" u SET "geomSimple" = ST_Multi(ST_CollectionExtract(s.g, 3))
		FROM (
			SELECT id, ST_CoverageSimplify(geom, $2) OVER () AS g FROM "GeoUnit" WHERE "uploadId" = $1
		) s
		WHERE u.id = s.id AND s.g IS NOT NULL AND NOT ST_IsEmpty(ST_CollectionExtract(s.g, 3))`, uploadID, tolerance)
	if err == nil {
		return savepoint.Commit(ctx)
	}
	p.Log.Warn("coverage simplification failed, simplifying each polygon", "uploadId", uploadID, "err", err)
	if err := savepoint.Rollback(ctx); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE "GeoUnit" SET "geomSimple" = ST_Multi(ST_CollectionExtract(ST_SimplifyPreserveTopology(geom, $2), 3))
		WHERE "uploadId" = $1`, uploadID, tolerance)
	return err
}

// adjacency stores the pairs of municipalities that share a border, between
// the new ones and with those already present in the same geographic area.
// Two municipalities are adjacent when their borders run within AdjacencyGapM
// of each other for at least AdjacencyMinBorderM: a corner contact is not enough.
func (p *Processor) adjacency(ctx context.Context, tx pgx.Tx, uploadID string, tree []string) (int64, error) {
	gap := p.Geo.AdjacencyGapM / metresPerDegree

	// Each new municipality grown by the gap, computed once.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE geo_grown ON COMMIT DROP AS
		SELECT id, ST_Buffer(geom, $2::float8, 'quad_segs=2') AS grown FROM "GeoUnit" WHERE "uploadId" = $1`, uploadID, gap); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `CREATE INDEX ON geo_grown USING gist (grown)`); err != nil {
		return 0, err
	}

	var ids []int32
	rows, err := tx.Query(ctx, `SELECT id FROM geo_grown ORDER BY id`)
	if err != nil {
		return 0, err
	}
	if ids, err = pgx.CollectRows(rows, pgx.RowTo[int32]); err != nil {
		return 0, err
	}

	// In chunks, so that progress moves on a large upload.
	const chunk = 250
	var total int64
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		tag, err := tx.Exec(ctx, `
			INSERT INTO "GeoAdjacency" ("aId", "bId", manual)
			SELECT LEAST(n.id, o.id), GREATEST(n.id, o.id), false
			FROM geo_grown n
			JOIN "GeoUnit" o ON o.geom && n.grown AND o.id <> n.id
			WHERE n.id BETWEEN $1 AND $2
				AND o."areaId" = ANY($3)
				-- A pair inside the upload is visited from its smaller id only.
				AND (o."uploadId" <> $4 OR o.id > n.id)
				AND ST_Length(ST_Intersection(ST_Boundary(o.geom), n.grown)::geography) >= $5
			ON CONFLICT DO NOTHING`,
			ids[start], ids[end-1], tree, uploadID, p.Geo.AdjacencyMinBorderM)
		if err != nil {
			return 0, err
		}
		total += tag.RowsAffected()
		p.progress(ctx, uploadID, 70+29*end/len(ids))
	}
	return total, nil
}

func names(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
