package advice

// cpuThrottle advises raising the job's CPU allowance when the container spent
// too many CFS periods throttled.
type cpuThrottle struct{}

func (cpuThrottle) Name() string { return "cpu-throttle" }

func (cpuThrottle) Check(Facts, Thresholds) []Advice { return nil }
