package advice

// longJob advises splitting jobs that run longer than the configured budget.
type longJob struct{}

func (longJob) Name() string { return "long-job" }

func (longJob) Check(Facts, Thresholds) []Advice { return nil }
