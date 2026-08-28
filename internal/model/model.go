// Package model defines the on-disk representation of a dumped database.
//
// A dump archive is a zip file whose manifest.json unmarshals into Manifest.
// The manifest is the single source of truth: the .sql files inside the archive
// are generated from it for inspection, and the importer regenerates the very
// same DDL from it at restore time.
package model

import (
	"strings"
	"time"
)

// FormatVersion is bumped whenever the archive layout changes incompatibly.
const FormatVersion = 1

// Manifest is the root document stored as manifest.json.
type Manifest struct {
	FormatVersion int       `json:"formatVersion"`
	Tool          string    `json:"tool"`
	CreatedAt     time.Time `json:"createdAt"`
	Source        Source    `json:"source"`
	Database      Database  `json:"database"`
}

// Source records where the dump came from, for traceability only.
type Source struct {
	Server        string `json:"server"`
	Database      string `json:"database"`
	ServerVersion string `json:"serverVersion"`
	Edition       string `json:"edition,omitempty"`
	Collation     string `json:"collation,omitempty"`
	SchemaOnly    bool   `json:"schemaOnly,omitempty"`
}

// Database holds every object we know how to reproduce.
type Database struct {
	Collation string     `json:"collation,omitempty"`
	Schemas   []Schema   `json:"schemas"`
	UserTypes []UserType `json:"userTypes,omitempty"`
	Sequences []Sequence `json:"sequences,omitempty"`
	Tables    []Table    `json:"tables"`
	Modules   []Module   `json:"modules,omitempty"`
}

// Schema is a SQL Server schema (namespace).
type Schema struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
}

// UserType is a user-defined scalar alias type or table type.
type UserType struct {
	Schema      string `json:"schema"`
	Name        string `json:"name"`
	IsTableType bool   `json:"isTableType"`

	// Scalar alias types.
	BaseType   string `json:"baseType,omitempty"`
	MaxLength  int    `json:"maxLength,omitempty"`
	Precision  int    `json:"precision,omitempty"`
	Scale      int    `json:"scale,omitempty"`
	IsNullable bool   `json:"isNullable,omitempty"`

	// Table types.
	Columns []Column `json:"columns,omitempty"`
}

// Sequence mirrors sys.sequences.
type Sequence struct {
	Schema       string `json:"schema"`
	Name         string `json:"name"`
	DataType     string `json:"dataType"`
	Precision    int    `json:"precision,omitempty"`
	Scale        int    `json:"scale,omitempty"`
	StartValue   string `json:"startValue"`
	Increment    string `json:"increment"`
	MinValue     string `json:"minValue,omitempty"`
	MaxValue     string `json:"maxValue,omitempty"`
	IsCycling    bool   `json:"isCycling,omitempty"`
	IsCached     bool   `json:"isCached,omitempty"`
	CacheSize    *int   `json:"cacheSize,omitempty"`
	CurrentValue string `json:"currentValue,omitempty"`
}

// Table is a user table plus everything attached to it.
type Table struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`

	PrimaryKey        *Index            `json:"primaryKey,omitempty"`
	UniqueConstraints []Index           `json:"uniqueConstraints,omitempty"`
	Indexes           []Index           `json:"indexes,omitempty"`
	ForeignKeys       []ForeignKey      `json:"foreignKeys,omitempty"`
	CheckConstraints  []CheckConstraint `json:"checkConstraints,omitempty"`

	IsMemoryOptimized bool `json:"isMemoryOptimized,omitempty"`

	// DataColumns lists, in file order, the columns present in DataFile.
	// Computed and rowversion columns are excluded: they cannot be inserted.
	DataColumns []string `json:"dataColumns,omitempty"`
	DataFile    string   `json:"dataFile,omitempty"`
	RowCount    int64    `json:"rowCount"`
	// EstimatedRows is the engine's own row count, read before the dump starts
	// so progress has something to count towards. It can be slightly stale, so
	// it drives display only, never control flow.
	EstimatedRows int64 `json:"estimatedRows,omitempty"`
	// DataSkipped records that the rows were deliberately left out of the
	// archive while the definition was kept. Foreign keys pointing at such a
	// table cannot be validated on restore.
	DataSkipped bool `json:"dataSkipped,omitempty"`
}

// QualifiedName returns the bracket-quoted [schema].[table].
func (t Table) QualifiedName() string { return Quote(t.Schema) + "." + Quote(t.Name) }

// Column describes one column of a table or table type.
type Column struct {
	Name    string `json:"name"`
	Ordinal int    `json:"ordinal"`

	// TypeSchema is empty for system types, otherwise the UDT's schema.
	TypeSchema string `json:"typeSchema,omitempty"`
	TypeName   string `json:"typeName"`
	// BaseTypeName is the underlying system type; equal to TypeName unless the
	// column uses a user-defined alias type. It drives value encoding.
	BaseTypeName string `json:"baseTypeName,omitempty"`
	// MaxLength is the raw sys.columns.max_length: bytes, with -1 meaning MAX.
	MaxLength  int    `json:"maxLength"`
	Precision  int    `json:"precision"`
	Scale      int    `json:"scale"`
	IsNullable bool   `json:"isNullable"`
	Collation  string `json:"collation,omitempty"`

	IsIdentity        bool   `json:"isIdentity,omitempty"`
	IdentitySeed      string `json:"identitySeed,omitempty"`
	IdentityIncrement string `json:"identityIncrement,omitempty"`

	IsComputed         bool   `json:"isComputed,omitempty"`
	ComputedDefinition string `json:"computedDefinition,omitempty"`
	IsPersisted        bool   `json:"isPersisted,omitempty"`

	DefaultName       string `json:"defaultName,omitempty"`
	DefaultDefinition string `json:"defaultDefinition,omitempty"`

	IsRowGUIDCol bool `json:"isRowGuidCol,omitempty"`
	IsSparse     bool `json:"isSparse,omitempty"`
}

// Insertable reports whether values for this column can be supplied by INSERT.
func (c Column) Insertable() bool {
	if c.IsComputed {
		return false
	}
	switch c.SystemType() {
	case "timestamp", "rowversion":
		return false
	}
	return true
}

// SystemType returns the underlying system type name, resolving alias types.
func (c Column) SystemType() string {
	if c.BaseTypeName != "" {
		return strings.ToLower(c.BaseTypeName)
	}
	return strings.ToLower(c.TypeName)
}

// IndexColumn is one key or included column of an index.
type IndexColumn struct {
	Name         string `json:"name"`
	IsDescending bool   `json:"isDescending,omitempty"`
	IsIncluded   bool   `json:"isIncluded,omitempty"`
}

// Index covers real indexes as well as PRIMARY KEY / UNIQUE constraints,
// which SQL Server also stores in sys.indexes.
type Index struct {
	Name    string `json:"name"`
	TypeDes string `json:"type"` // CLUSTERED, NONCLUSTERED, CLUSTERED COLUMNSTORE, ...

	IsPrimaryKey       bool `json:"isPrimaryKey,omitempty"`
	IsUniqueConstraint bool `json:"isUniqueConstraint,omitempty"`
	IsUnique           bool `json:"isUnique,omitempty"`

	Columns          []IndexColumn `json:"columns"`
	FilterDefinition string        `json:"filterDefinition,omitempty"`
	FillFactor       int           `json:"fillFactor,omitempty"`
	IsPadded         bool          `json:"isPadded,omitempty"`
	IgnoreDupKey     bool          `json:"ignoreDupKey,omitempty"`
	IsDisabled       bool          `json:"isDisabled,omitempty"`
}

// ForeignKey mirrors sys.foreign_keys.
type ForeignKey struct {
	Name              string   `json:"name"`
	Columns           []string `json:"columns"`
	ReferencedSchema  string   `json:"referencedSchema"`
	ReferencedTable   string   `json:"referencedTable"`
	ReferencedColumns []string `json:"referencedColumns"`
	OnDelete          string   `json:"onDelete,omitempty"` // NO ACTION | CASCADE | SET NULL | SET DEFAULT
	OnUpdate          string   `json:"onUpdate,omitempty"`
	IsDisabled        bool     `json:"isDisabled,omitempty"`
	IsNotTrusted      bool     `json:"isNotTrusted,omitempty"`
}

// CheckConstraint mirrors sys.check_constraints.
type CheckConstraint struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	IsDisabled bool   `json:"isDisabled,omitempty"`
}

// ModuleKind groups sys.sql_modules objects by how they must be ordered.
type ModuleKind string

const (
	ModuleView      ModuleKind = "view"
	ModuleFunction  ModuleKind = "function"
	ModuleProcedure ModuleKind = "procedure"
	ModuleTrigger   ModuleKind = "trigger"
)

// Module is a view, function, stored procedure or trigger, stored verbatim.
type Module struct {
	Schema     string     `json:"schema"`
	Name       string     `json:"name"`
	Kind       ModuleKind `json:"kind"`
	Definition string     `json:"definition"`
	// ParentSchema and ParentName identify the table a trigger is attached to.
	ParentSchema        string `json:"parentSchema,omitempty"`
	ParentName          string `json:"parentName,omitempty"`
	AnsiNulls           bool   `json:"ansiNulls"`
	QuotedIdentifier    bool   `json:"quotedIdentifier"`
	IsDisabled          bool   `json:"isDisabled,omitempty"`
	IsSchemaBound       bool   `json:"isSchemaBound,omitempty"`
	ExecuteAsPrincipal  string `json:"executeAsPrincipal,omitempty"`
	IsInsteadOfTrigger  bool   `json:"isInsteadOfTrigger,omitempty"`
	OrderHintPreference int    `json:"-"`
}
