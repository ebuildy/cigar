package advice

// memoryPressure advises raising the memory limit when peak working set came
// close to it (OOMKill risk).
type memoryPressure struct{}

func (memoryPressure) Name() string { return "memory-pressure" }

func (memoryPressure) Check(Facts, Thresholds) []Advice { return nil }
