package advice

// registry is every known rule, in report order: the built-ins first, then
// anything added by Register.
//
// Adding an adviser is one new file implementing Rule plus one line here.
var registry = []Rule{
	cpuThrottle{},
	javaThreads{},
	longJob{},
	memoryPressure{},
}

// Register appends a rule to the registry so New can select it. It exists so
// rules can be added without editing this file — call it from an init in the
// rule's own file, or from wiring code.
func Register(r Rule) {
	registry = append(registry, r)
}
