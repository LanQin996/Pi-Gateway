package store

// Shared protocol literals keep schema and validation messages consistent.
const (
	textEmptyColumn   = "TEXT NOT NULL DEFAULT ''"
	textArrayColumn   = "TEXT NOT NULL DEFAULT '[]'"
	integerZeroColumn = "INTEGER NOT NULL DEFAULT 0"
	realZeroColumn    = "REAL NOT NULL DEFAULT 0"
	sqliteMemoryPath  = ":memory:"
)
