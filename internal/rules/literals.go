package rules

// Shared protocol literals keep schema and validation messages consistent.
const (
	valueFieldLabel           = "值 / Value"
	sourceFieldLabel          = "来源 / Source"
	metadataPointer           = "/metadata"
	inputPointer              = "/input"
	contentPointer            = "/content"
	toolsPointer              = "/tools"
	functionsParameterPointer = "/params/functions"
	trailingJSONValueMessage  = "trailing JSON value"
	unsafeIntegerMessage      = "integer exceeds JavaScript safe range"
	valuePointer              = "/value"
)
