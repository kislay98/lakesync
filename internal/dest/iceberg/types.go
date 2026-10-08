package iceberg

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"

	"lakesync/internal/core"
)

// icebergType maps a core column type to its Iceberg type.
func icebergType(t core.ColumnType) (iceberg.Type, error) {
	switch t {
	case core.TypeString:
		return iceberg.PrimitiveTypes.String, nil
	case core.TypeLong:
		return iceberg.PrimitiveTypes.Int64, nil
	case core.TypeDouble:
		return iceberg.PrimitiveTypes.Float64, nil
	case core.TypeBoolean:
		return iceberg.PrimitiveTypes.Bool, nil
	case core.TypeTimestampTz:
		return iceberg.PrimitiveTypes.TimestampTz, nil
	case core.TypeBinary:
		return iceberg.PrimitiveTypes.Binary, nil
	}
	return nil, fmt.Errorf("unsupported column type %q", t)
}

// coreType maps an Iceberg type back to a core column type. Unsupported types
// (never created by this app) degrade to string and are reported.
func coreType(t iceberg.Type) (core.ColumnType, bool) {
	switch t.(type) {
	case iceberg.StringType:
		return core.TypeString, true
	case iceberg.Int64Type, iceberg.Int32Type:
		return core.TypeLong, true
	case iceberg.Float64Type, iceberg.Float32Type:
		return core.TypeDouble, true
	case iceberg.BooleanType:
		return core.TypeBoolean, true
	case iceberg.TimestampTzType, iceberg.TimestampType:
		return core.TypeTimestampTz, true
	case iceberg.BinaryType:
		return core.TypeBinary, true
	}
	return core.TypeString, false
}

// arrowType maps an Iceberg type to the Arrow type used for writing.
func arrowType(t iceberg.Type) (arrow.DataType, error) {
	switch t.(type) {
	case iceberg.StringType:
		return arrow.BinaryTypes.String, nil
	case iceberg.Int64Type:
		return arrow.PrimitiveTypes.Int64, nil
	case iceberg.Float64Type:
		return arrow.PrimitiveTypes.Float64, nil
	case iceberg.BooleanType:
		return arrow.FixedWidthTypes.Boolean, nil
	case iceberg.TimestampTzType:
		return &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, nil
	case iceberg.BinaryType:
		return arrow.BinaryTypes.Binary, nil
	}
	return nil, fmt.Errorf("cannot write Iceberg type %s", t)
}

// arrowSchema builds the Arrow schema matching an Iceberg schema.
func arrowSchema(sc *iceberg.Schema) (*arrow.Schema, error) {
	fields := make([]arrow.Field, 0, len(sc.Fields()))
	for _, f := range sc.Fields() {
		dt, err := arrowType(f.Type)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", f.Name, err)
		}
		fields = append(fields, arrow.Field{Name: f.Name, Type: dt, Nullable: !f.Required})
	}
	return arrow.NewSchema(fields, nil), nil
}

// rowsToRecord converts flattened rows into one Arrow record following the
// table's current schema. User columns are matched by column name; table
// columns missing from the batch schema become NULL.
func rowsToRecord(tableSchema *iceberg.Schema, batch *core.Batch) (arrow.RecordBatch, error) {
	asc, err := arrowSchema(tableSchema)
	if err != nil {
		return nil, err
	}
	batchIndex := make(map[string]int, len(batch.Schema.Columns))
	for i, c := range batch.Schema.Columns {
		batchIndex[c.Name] = i
	}
	tableCols := map[string]bool{}
	for _, f := range tableSchema.Fields() {
		tableCols[f.Name] = true
	}
	for _, c := range batch.Schema.Columns {
		if !tableCols[c.Name] {
			return nil, fmt.Errorf("batch column %q is not in the table schema; evolve the schema first", c.Name)
		}
	}
	b := array.NewRecordBuilder(memory.DefaultAllocator, asc)
	defer b.Release()
	for i, f := range asc.Fields() {
		fb := b.Field(i)
		if core.IsMetaColumn(f.Name) {
			for r := range batch.Rows {
				appendMeta(fb, f.Name, &batch.Rows[r])
			}
			continue
		}
		idx, known := batchIndex[f.Name]
		for r := range batch.Rows {
			if !known || idx >= len(batch.Rows[r].Values) {
				fb.AppendNull()
				continue
			}
			appendValue(fb, batch.Rows[r].Values[idx])
		}
	}
	return b.NewRecordBatch(), nil
}

func appendMeta(fb array.Builder, name string, row *core.Row) {
	switch name {
	case core.ColKey:
		fb.(*array.StringBuilder).Append(row.Key)
	case core.ColOp:
		fb.(*array.StringBuilder).Append(string(row.Op))
	case core.ColSeq:
		fb.(*array.Int64Builder).Append(row.Seq)
	case core.ColGeneration:
		fb.(*array.Int64Builder).Append(row.Generation)
	case core.ColSourceTS:
		if row.SourceTS.IsZero() {
			fb.AppendNull()
		} else {
			fb.(*array.TimestampBuilder).Append(arrow.Timestamp(row.SourceTS.UnixMicro()))
		}
	case core.ColIngestedAt:
		fb.(*array.TimestampBuilder).Append(arrow.Timestamp(row.IngestedAt.UnixMicro()))
	case core.ColCastErrors:
		if row.CastErrors == "" {
			fb.AppendNull()
		} else {
			fb.(*array.StringBuilder).Append(row.CastErrors)
		}
	case core.ColDoc:
		if row.Doc == "" {
			fb.AppendNull()
		} else {
			fb.(*array.StringBuilder).Append(row.Doc)
		}
	default:
		fb.AppendNull()
	}
}

// appendValue writes a core value into a builder of the matching Arrow type.
// Mismatched kinds (which Flatten already turned into NULL) become NULL.
func appendValue(fb array.Builder, v core.Value) {
	if v.IsNull() {
		fb.AppendNull()
		return
	}
	switch b := fb.(type) {
	case *array.StringBuilder:
		switch v.Kind {
		case core.KindString, core.KindDecimal, core.KindJSON, core.KindOther:
			b.Append(v.Str)
		default:
			b.AppendNull()
		}
	case *array.Int64Builder:
		if v.Kind == core.KindInt {
			b.Append(v.Int)
		} else {
			b.AppendNull()
		}
	case *array.Float64Builder:
		switch v.Kind {
		case core.KindFloat:
			b.Append(v.Float)
		case core.KindInt:
			b.Append(float64(v.Int))
		default:
			b.AppendNull()
		}
	case *array.BooleanBuilder:
		if v.Kind == core.KindBool {
			b.Append(v.Bool)
		} else {
			b.AppendNull()
		}
	case *array.TimestampBuilder:
		if v.Kind == core.KindTime {
			b.Append(arrow.Timestamp(v.Time.UnixMicro()))
		} else {
			b.AppendNull()
		}
	case *array.BinaryBuilder:
		if v.Kind == core.KindBinary {
			b.Append(v.Bytes)
		} else {
			b.AppendNull()
		}
	default:
		fb.AppendNull()
	}
}

// userColumns extracts the user columns of an Iceberg schema as a core schema.
func userColumns(sc *iceberg.Schema, log *slog.Logger) core.Schema {
	var out core.Schema
	for _, f := range sc.Fields() {
		if strings.HasPrefix(f.Name, core.MetaPrefix) {
			continue
		}
		t, ok := coreType(f.Type)
		if !ok && log != nil {
			log.Warn("unsupported Iceberg column type treated as string", "column", f.Name, "type", f.Type.String())
		}
		src := f.Doc
		if src == "" {
			src = f.Name
		}
		out.Columns = append(out.Columns, core.Column{Name: f.Name, SourceField: src, Type: t})
	}
	return out
}

// snapshotTime converts a snapshot timestamp.
func snapshotTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
