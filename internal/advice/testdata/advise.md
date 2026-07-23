### Advice for pipeline #12345

#### `compile`

**⚠️ CPU throttling**

This job spent **41%** of its CPU periods throttled, against a limit of 500m. The runner had less CPU than the job asked for, so wall-clock time is inflated.

Raise the allowance with GitLab CI variables, on the job or on the project:

```yaml
variables:
  KUBERNETES_CPU_REQUEST: "250m"
  KUBERNETES_CPU_LIMIT: "1000m"
```

**☕ Java build parallelism**

The trace shows a **Maven** build. Maven's `-T 1C` and Gradle's default worker count both size themselves from the *host* core count, and the JVM only derives `Runtime.availableProcessors()` from the cgroup quota when a CPU **limit** is set. With requests only, a JVM on a 64-core node builds 64-wide thread pools inside a one-core slice — which is exactly what shows up as CFS throttling.

Cap the parallelism to the CPU the pod actually gets (1):

```sh
mvn -T 1 verify   # an explicit count, not -T 1C
```

If tests fork JVMs, cap Surefire's `forkCount` too, and pin the JVM's view of the machine with `JAVA_TOOL_OPTIONS=-XX:ActiveProcessorCount=1`. Never set `-XX:-UseContainerSupport` — that disables container awareness entirely.

That 1 matches the pod's current limit of 500m. If you also raise the CPU limit, raise this count to match.

- <https://cwiki.apache.org/confluence/display/MAVEN/Parallel+builds+in+Maven+3>
- <https://docs.gradle.org/current/userguide/command_line_interface.html>
- <https://kestra.io/docs/administrator-guide/jvm-cpu-limits>

**⏱️ Long job**

This job ran for **22m0s**, over the 10m0s budget. It is the pipeline's critical path: every push waits on it.

If the work splits, split it — several jobs in one stage run in parallel on separate runners:

- Break independent steps (lint, unit, integration) into separate jobs.
- Shard a long test suite with `parallel:` (or `parallel:matrix:`) and a shard flag.
- Move setup that repeats every run into `cache:` or a prebuilt image.

**🧠 Memory near the limit**

Peak working set reached **480Mi of the 512Mi limit (94%)**. A job this close to its limit gets OOMKilled the moment a build step allocates a little more — and an OOMKill looks like a flaky failure, not a resource problem.

Raise the ceiling with GitLab CI variables:

```yaml
variables:
  KUBERNETES_MEMORY_REQUEST: "256Mi"
  KUBERNETES_MEMORY_LIMIT: "768Mi"
```

