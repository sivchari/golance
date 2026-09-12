package auditfeat

// Level is a logging severity level.
type Level int

// Level values, from least to most severe.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)
