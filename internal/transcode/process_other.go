//go:build !linux

package transcode

// Runtime CPU/RSS sampling is currently available on the Linux deployment.
func sampleProcess(int) processSample { return processSample{} }
