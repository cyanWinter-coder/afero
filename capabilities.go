package afero

// Operation identifies a class of filesystem operations. Wrappers such as
// ReadOnlyFs and RegexpFs use it to declare which operations they pass
// through to their source and which they deny themselves.
type Operation uint32

const (
	OpStat Operation = 1 << iota
	OpLstat
	OpReadDir
	// OpOpen covers opening existing files for reading.
	OpOpen
	OpReadlink
	OpCreate
	// OpOpenWrite covers OpenFile with any of the write flags
	// (O_WRONLY, O_RDWR, O_APPEND, O_CREATE, O_TRUNC).
	OpOpenWrite
	OpMkdir
	OpRemove
	OpRename
	OpChmod
	OpChown
	OpChtimes
	OpSymlink
)

const (
	// CapabilityRead groups the operations that only observe filesystem
	// state.
	CapabilityRead Operation = OpStat | OpLstat | OpReadDir | OpOpen | OpReadlink
	// CapabilityWrite groups the operations that mutate filesystem state.
	CapabilityWrite Operation = OpCreate | OpOpenWrite | OpMkdir | OpRemove |
		OpRename | OpChmod | OpChown | OpChtimes | OpSymlink
	// CapabilityAll is the capability set of an unrestricted Fs.
	CapabilityAll Operation = CapabilityRead | CapabilityWrite
)

// CapabilityReporter is implemented by Fs wrappers that restrict the
// operations supported by their source. The reported set must never contain
// an operation the wrapped Fs does not support: composition narrows
// capabilities, it never elevates them.
type CapabilityReporter interface {
	// Capabilities returns the operations supported by this wrapper,
	// intersected with the capabilities of its source.
	Capabilities() Operation
}

// DeniedErrorMapper is implemented by Fs wrappers that deny operations
// themselves (rather than passing them to their source). It declares the
// wrapper's error mapping: the single error a caller will observe for an
// operation the wrapper denied.
type DeniedErrorMapper interface {
	// DeniedError returns the error reported for an operation this
	// wrapper denies without consulting its source.
	DeniedError(op Operation) error
}

// CapabilitiesOf returns the effective capability set of fs. Wrappers
// implementing CapabilityReporter report their (already intersected) set;
// any other Fs is assumed to support every operation.
func CapabilitiesOf(fs Fs) Operation {
	if cr, ok := fs.(CapabilityReporter); ok {
		return cr.Capabilities()
	}
	return CapabilityAll
}

// Supports reports whether fs supports op through its entire wrapper chain.
func Supports(fs Fs, op Operation) bool {
	return CapabilitiesOf(fs)&op == op
}
