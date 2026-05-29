// Package pipeline provides the data pipeline stages: ingest, validate,
// normalise, and enrich. Used by cmd/pipeline to process publisher data files.
//
// Each stage takes records in and produces records out, with bad records
// routed to quarantine. Stages are composable - chain them together.
//
// Usage:
//
//	p := pipeline.New(logger)
//	result := p.Process(ctx, records, config)
package pipeline

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Record is a single row of publisher data as key-value pairs.
type Record map[string]string

// ValidationError describes why a record failed validation.
type ValidationError struct {
	Field   string
	Value   string
	Rule    string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("field %s: %s (value=%q, rule=%s)", e.Field, e.Message, e.Value, e.Rule)
}

// Result holds the output of a pipeline run.
type Result struct {
	Valid       []Record
	Quarantine []QuarantineRecord
	Stats      Stats
}

// QuarantineRecord is a failed record with its errors.
type QuarantineRecord struct {
	Record Record
	Errors []ValidationError
}

// Stats tracks pipeline run metrics.
type Stats struct {
	TotalInput    int
	Valid         int
	Quarantined   int
	Duration      time.Duration
	SchemaVersion int
}

// PublisherConfig describes a publisher's data format.
type PublisherConfig struct {
	PublisherID    string
	Format         string // csv, tsv, json
	Delimiter      rune
	RequiredFields []string
	FieldMappings  map[string]string // publisher_field -> canonical_field
	DateFormat     string
}

// Pipeline runs data through ingest -> validate -> normalise -> enrich.
type Pipeline struct {
	log *slog.Logger
}

// New creates a pipeline.
func New(log *slog.Logger) *Pipeline {
	return &Pipeline{log: log}
}

// Process runs all stages on the input records.
func (p *Pipeline) Process(ctx context.Context, records []Record, cfg PublisherConfig) Result {
	start := time.Now()
	result := Result{
		Stats: Stats{TotalInput: len(records), SchemaVersion: 1},
	}

	for _, rec := range records {
		// Stage 1: Normalise field names
		normalised := normalise(rec, cfg.FieldMappings)

		// Stage 2: Validate required fields
		errs := validate(normalised, cfg.RequiredFields)
		if len(errs) > 0 {
			result.Quarantine = append(result.Quarantine, QuarantineRecord{
				Record: normalised,
				Errors: errs,
			})
			continue
		}

		// Stage 3: Enrich
		enriched := enrich(normalised)

		result.Valid = append(result.Valid, enriched)
	}

	result.Stats.Valid = len(result.Valid)
	result.Stats.Quarantined = len(result.Quarantine)
	result.Stats.Duration = time.Since(start)

	p.log.Info("pipeline complete",
		"total", result.Stats.TotalInput,
		"valid", result.Stats.Valid,
		"quarantined", result.Stats.Quarantined,
		"duration_ms", result.Stats.Duration.Milliseconds(),
	)

	return result
}

// IngestCSV reads CSV data and produces records.
func IngestCSV(r io.Reader, delimiter rune) ([]Record, error) {
	reader := csv.NewReader(r)
	reader.Comma = delimiter
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read headers: %w", err)
	}

	// Normalise headers
	for i, h := range headers {
		headers[i] = strings.TrimSpace(strings.ToLower(h))
	}

	var records []Record
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed rows
		}

		rec := make(Record, len(headers))
		for i, h := range headers {
			if i < len(row) {
				rec[h] = strings.TrimSpace(row[i])
			}
		}
		records = append(records, rec)
	}

	return records, nil
}

// normalise maps publisher-specific field names to canonical names.
func normalise(rec Record, mappings map[string]string) Record {
	if len(mappings) == 0 {
		return rec
	}

	out := make(Record, len(rec))
	for k, v := range rec {
		if canonical, ok := mappings[k]; ok {
			out[canonical] = v
		} else {
			out[k] = v
		}
	}
	return out
}

// validate checks required fields are present and non-empty.
func validate(rec Record, required []string) []ValidationError {
	var errs []ValidationError
	for _, field := range required {
		val, ok := rec[field]
		if !ok || val == "" {
			errs = append(errs, ValidationError{
				Field:   field,
				Value:   val,
				Rule:    "required",
				Message: "field is required but missing or empty",
			})
		}
	}
	return errs
}

// enrich adds derived fields to a record.
func enrich(rec Record) Record {
	// Add processing timestamp
	rec["_processed_at"] = time.Now().UTC().Format(time.RFC3339)

	// Normalise geo codes to uppercase
	if geo, ok := rec["geo"]; ok {
		rec["geo"] = strings.ToUpper(geo)
	}

	// Normalise device to lowercase
	if device, ok := rec["device"]; ok {
		rec["device"] = strings.ToLower(device)
	}

	return rec
}
