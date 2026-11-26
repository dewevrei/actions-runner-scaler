package docker

import "math"

// Capacity is what the Docker daemon reports about its host (for Docker
// Desktop, the VM): the CPU count and the memory in bytes.
type Capacity struct {
	NCPU     int
	MemTotal int64
}

// Share is what one runner reserves: the CPU and memory limits of its runner
// container and its BuildKit container added together.
type Share struct {
	// NanoCPUs is the CPU limit in units of 1e-9 CPU.
	NanoCPUs int64
	// Memory is the memory limit in bytes.
	Memory int64
}

// RunnerShare returns the share of one runner, given the limits of its
// runner and BuildKit containers. CPUs convert to NanoCPUs with the same
// rounding as the container spec, so the share equals what Docker is
// asked to enforce (0002 SPEC R201).
func RunnerShare(runner, buildKit Limits) Share {
	return Share{
		NanoCPUs: nanoCPUs(runner.CPUs) + nanoCPUs(buildKit.CPUs),
		Memory:   int64(runner.Memory) + int64(buildKit.Memory),
	}
}

// nanoCPUs converts a CPU count to the integer NanoCPUs Docker takes. The
// container spec and the capacity check both use it, so the share equals
// what Docker is asked to enforce.
func nanoCPUs(cpus float64) int64 { return int64(math.Round(cpus * 1e9)) }

// ServerLimit returns how many runners of share s fit in c: the smaller of
// the CPU and the memory quotient, each rounded down. Both divide integers,
// because a float quotient could come out one too small for sums such as
// 0.5 + 1 CPU. It is 0 when either part of s is zero or negative, and when
// a share does not fit even once (0002 SPEC R201, R202).
func ServerLimit(c Capacity, s Share) int {
	if s.NanoCPUs <= 0 || s.Memory <= 0 || c.NCPU <= 0 || c.MemTotal <= 0 {
		return 0
	}
	byCPU := int64(c.NCPU) * 1e9 / s.NanoCPUs
	byMemory := c.MemTotal / s.Memory
	return int(min(byCPU, byMemory))
}
