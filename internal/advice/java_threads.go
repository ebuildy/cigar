package advice

// javaThreads advises pinning Maven/Gradle/JVM parallelism to the pod's CPU
// limit when a throttled job's trace shows a Java build.
type javaThreads struct{}

func (javaThreads) Name() string { return "java-threads" }

func (javaThreads) NeedsTrace(Facts, Thresholds) bool { return false }

func (javaThreads) Check(Facts, Thresholds) []Advice { return nil }
